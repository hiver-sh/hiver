package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	MntDir    = "/mnt"
	RootfsDir = MntDir + "/rootfs"
	// ScratchDir is a tmpfs mounted by MountOverlay so that UpperDir and
	// WorkDir are never on an overlay filesystem. Docker's overlay2 storage
	// driver backs the container's root (including /mnt), and the kernel
	// rejects EINVAL when upper/work are themselves on overlayfs.
	ScratchDir = MntDir + "/scratch"
	UpperDir   = ScratchDir + "/upper"
	WorkDir    = ScratchDir + "/work"
	MergedDir  = MntDir + "/merged"
)

// ImageConfig is the subset of the Docker / OCI image config that
// sandboxd needs in order to build a runtime spec for the agent.
type ImageConfig struct {
	Entrypoint  []string
	Cmd         []string
	Env         []string
	WorkingDir  string
	ExposedPort *string
}

// dockerArchiveManifest mirrors the top-level manifest.json that
// `docker save` writes inside its tarball.
type dockerArchiveManifest []struct {
	Config string   `json:"Config"`
	Layers []string `json:"Layers"`
}

type dockerImageConfig struct {
	Config struct {
		ExposedPorts map[string]any `json:"ExposedPorts"`
		Entrypoint   []string       `json:"Entrypoint"`
		Cmd          []string       `json:"Cmd"`
		Env          []string       `json:"Env"`
		WorkingDir   string         `json:"WorkingDir"`
	} `json:"config"`
}

func ExtractImageConfig() (*ImageConfig, error) {
	manifestData, err := os.ReadFile(filepath.Join(MntDir, "manifest.json"))
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

	var dic dockerImageConfig
	if img.Config != "" {
		cfgData, err := os.ReadFile(filepath.Join(MntDir, "rootfs", img.Config))
		if err != nil {
			return nil, fmt.Errorf("read image config %s: %w", img.Config, err)
		}
		if err := json.Unmarshal(cfgData, &dic); err != nil {
			return nil, fmt.Errorf("parse image config: %w", err)
		}
	}

	exposedPort := findExposedTcpPort(dic.Config.ExposedPorts)

	return &ImageConfig{
		Entrypoint:  dic.Config.Entrypoint,
		Cmd:         dic.Config.Cmd,
		Env:         dic.Config.Env,
		WorkingDir:  dic.Config.WorkingDir,
		ExposedPort: exposedPort,
	}, nil
}

func findExposedTcpPort(exposedPorts map[string]any) *string {
	if len(exposedPorts) == 0 {
		return nil
	}

	for key, _ := range exposedPorts {
		portParts := strings.Split(key, "/")
		port, protocol := portParts[0], portParts[1]
		if protocol == "tcp" {
			return &port
		}
	}
	return nil
}
