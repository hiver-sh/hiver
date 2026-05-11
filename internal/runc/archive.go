// Package runc converts a docker-archive tarball (the output of
// `docker save`) into an OCI runtime bundle and runs it under runc.
//
// Why no skopeo / umoci dependency? We only need to consume
// images produced by the local Docker daemon, and `docker save` emits a
// well-defined archive format we can parse in ~150 lines. Pulling in
// skopeo+umoci would mean ~80 MB of apt packages and a multi-step
// pipeline (skopeo copy → umoci unpack) for no functional gain.
package runc

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImageConfig is the subset of the Docker / OCI image config that
// sandboxd needs in order to build a runtime spec for the agent.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// dockerArchiveManifest mirrors the top-level manifest.json that
// `docker save` writes inside its tarball.
type dockerArchiveManifest []struct {
	Config string   `json:"Config"`
	Layers []string `json:"Layers"`
}

type dockerImageConfig struct {
	Config struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}

// ExtractDockerArchive reads a docker-archive tarball, unpacks the
// image's layered rootfs into rootfsDir (in layer order, on top of
// itself), and returns the parsed image config.
func ExtractDockerArchive(archivePath, rootfsDir string) (*ImageConfig, error) {
	staging, err := os.MkdirTemp("", "agent-stage-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	if err := extractTarFile(archivePath, staging); err != nil {
		return nil, fmt.Errorf("stage archive: %w", err)
	}

	manifestData, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	var manifest dockerArchiveManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("manifest.json: no images")
	}
	img := manifest[0]

	cfgData, err := os.ReadFile(filepath.Join(staging, img.Config))
	if err != nil {
		return nil, fmt.Errorf("read image config %s: %w", img.Config, err)
	}
	var dic dockerImageConfig
	if err := json.Unmarshal(cfgData, &dic); err != nil {
		return nil, fmt.Errorf("parse image config: %w", err)
	}

	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return nil, err
	}
	for _, layer := range img.Layers {
		if err := extractTarFile(filepath.Join(staging, layer), rootfsDir); err != nil {
			return nil, fmt.Errorf("extract layer %s: %w", layer, err)
		}
	}

	return &ImageConfig{
		Entrypoint: dic.Config.Entrypoint,
		Cmd:        dic.Config.Cmd,
		Env:        dic.Config.Env,
		WorkingDir: dic.Config.WorkingDir,
	}, nil
}

func extractTarFile(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return extractTarReader(f, destDir)
}

// extractTarReader walks a tar (transparently decompressed if gzipped)
// and materializes its entries under destDir. Device nodes and unknown
// types are skipped — runc populates /dev itself, and the images don't
// carry whiteouts.
func extractTarReader(r io.Reader, destDir string) error {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(2)
	var src io.Reader = br
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		src = gz
	}
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Skip parent dir and absolute paths to avoid escapes.
		clean := filepath.Clean(h.Name)
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
			continue
		}
		target := filepath.Join(destDir, clean)
		mode := os.FileMode(h.Mode) & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			_ = os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(destDir, h.Linkname), target); err != nil {
				return err
			}
		}
	}
	return nil
}
