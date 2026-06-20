package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// ReadyFifoPath is a named pipe on the scratch tmpfs that the agent
	// container's poststart hook writes to once its entrypoint is running.
	// It lives in the runtime (host) mount namespace — where runc executes
	// hooks — not inside the container's root (MergedDir), so the hook and
	// sandboxd's WaitReady both see it. See WriteConfig's poststart hook.
	ReadyFifoPath = ScratchDir + "/ready.fifo"
)

// Overlay locates the directories of one agent's overlayfs stack. Lower is the
// read-only image rootfs (shared across all sandboxes of the same image); the
// rest are per-sandbox writable paths. Packing N sandboxes into one pod gives
// each its own Scratch/Upper/Work/Merged while sharing Lower.
type Overlay struct {
	Lower   string // base image, read-only (shared)
	Scratch string // tmpfs root holding Upper+Work (kept off any overlayfs)
	Upper   string // sandbox writes
	Work    string // overlayfs scratch
	Merged  string // unified view; becomes runc's root.path
}

// DefaultOverlay is the single-sandbox layout under /mnt that sandboxd has
// always used; the boot sandbox keeps it so its behavior is unchanged.
func DefaultOverlay() Overlay {
	return Overlay{Lower: RootfsDir, Scratch: ScratchDir, Upper: UpperDir, Work: WorkDir, Merged: MergedDir}
}

// ImageConfig is the subset of the Docker / OCI image config that
// sandboxd needs in order to build a runtime spec for the agent.
type ImageConfig struct {
	Entrypoint  []string
	Cmd         []string
	Env         []string
	WorkingDir  string
	ExposedPort *string
	// ExposedPorts is every TCP port the image declares via EXPOSE, ascending.
	ExposedPorts []int
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
		// The bundler relocates the OCI blobs to /mnt/blobs — out of the rootfs
		// that becomes the workload root — so the image-distribution scaffolding
		// (blobs/, index.json, …) doesn't leak into the guest. Resolve the config
		// there, falling back to the in-rootfs path for older/legacy bundles.
		cfgData, err := os.ReadFile(filepath.Join(MntDir, img.Config))
		if os.IsNotExist(err) {
			cfgData, err = os.ReadFile(filepath.Join(RootfsDir, img.Config))
		}
		if err != nil {
			return nil, fmt.Errorf("read image config %s: %w", img.Config, err)
		}
		if err := json.Unmarshal(cfgData, &dic); err != nil {
			return nil, fmt.Errorf("parse image config: %w", err)
		}
	}

	exposedPort := findExposedTcpPort(dic.Config.ExposedPorts)

	return &ImageConfig{
		Entrypoint:   dic.Config.Entrypoint,
		Cmd:          dic.Config.Cmd,
		Env:          dic.Config.Env,
		WorkingDir:   dic.Config.WorkingDir,
		ExposedPort:  exposedPort,
		ExposedPorts: findExposedTcpPorts(dic.Config.ExposedPorts),
	}, nil
}

// findExposedTcpPorts returns every TCP port in the image's ExposedPorts set
// (the Dockerfile EXPOSE directives), ascending. Keys are "port/proto"; UDP
// and malformed entries are skipped. Never nil, so callers serialize an empty
// set as `[]`.
func findExposedTcpPorts(exposedPorts map[string]any) []int {
	ports := make([]int, 0, len(exposedPorts))
	for key := range exposedPorts {
		portStr, protocol, ok := strings.Cut(key, "/")
		if !ok || protocol != "tcp" {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func findExposedTcpPort(exposedPorts map[string]any) *string {
	if len(exposedPorts) == 0 {
		return nil
	}

	for key := range exposedPorts {
		portParts := strings.Split(key, "/")
		port, protocol := portParts[0], portParts[1]
		if protocol == "tcp" {
			return &port
		}
	}
	return nil
}
