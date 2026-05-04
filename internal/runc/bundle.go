package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// BundleParams configure the OCI runtime spec sandboxd writes for the
// agent container. The bundle layout matches what `runc run -b <dir>`
// expects: <dir>/config.json + <dir>/rootfs/.
type BundleParams struct {
	BundleDir   string       // bundle root — must already contain rootfs/
	ImageConfig *ImageConfig // image-supplied entrypoint/cmd/env/cwd
	ExtraEnv    []string     // sandboxd-injected KEY=VAL entries
	Mounts      []BindMount  // additional host bind mounts (e.g. /workspace)
	Hostname    string
}

// BindMount represents a host→container bind. Source is interpreted in
// runc's mount namespace (i.e. inside the sandbox-pod container).
type BindMount struct {
	Source      string
	Destination string
	Options     []string // e.g. {"bind","rw"} or {"bind","ro"}
}

// WriteConfig generates config.json under p.BundleDir.
//
// Process args = image entrypoint + image cmd. Env = image env + extra.
// Network namespace is intentionally NOT created — the agent inherits
// the sandbox-pod's netns so 127.0.0.1:<sbxproxy-port> resolves and
// HTTP_PROXY traffic is mediated. Pid/mount/ipc/uts ARE new.
func WriteConfig(p BundleParams) error {
	args := append([]string{}, p.ImageConfig.Entrypoint...)
	args = append(args, p.ImageConfig.Cmd...)
	if len(args) == 0 {
		return fmt.Errorf("agent image has no entrypoint or cmd")
	}
	env := append([]string{}, p.ImageConfig.Env...)
	env = append(env, p.ExtraEnv...)
	cwd := p.ImageConfig.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	caps := []string{
		"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER",
		"CAP_FSETID", "CAP_KILL", "CAP_MKNOD", "CAP_NET_BIND_SERVICE",
		"CAP_NET_RAW", "CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP",
		"CAP_SETUID", "CAP_SYS_CHROOT",
	}

	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{"destination": "/dev/pts", "type": "devpts", "source": "devpts",
			"options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{"destination": "/dev/shm", "type": "tmpfs", "source": "shm",
			"options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{"destination": "/dev/mqueue", "type": "mqueue", "source": "mqueue",
			"options": []string{"nosuid", "noexec", "nodev"}},
		{"destination": "/sys", "type": "sysfs", "source": "sysfs",
			"options": []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	for _, m := range p.Mounts {
		mounts = append(mounts, map[string]any{
			"destination": m.Destination,
			"type":        "bind",
			"source":      m.Source,
			"options":     append([]string{"bind"}, m.Options...),
		})
	}

	spec := map[string]any{
		"ociVersion": "1.0.2-dev",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     args,
			"env":      env,
			"cwd":      cwd,
			"capabilities": map[string]any{
				"bounding":    caps,
				"effective":   caps,
				"inheritable": caps,
				"permitted":   caps,
				"ambient":     caps,
			},
			"rlimits": []map[string]any{
				{"type": "RLIMIT_NOFILE", "hard": 1024, "soft": 1024},
			},
			"noNewPrivileges": true,
		},
		"root":     map[string]any{"path": "rootfs", "readonly": false},
		"hostname": p.Hostname,
		"mounts":   mounts,
		"linux": map[string]any{
			"namespaces": []map[string]string{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				// network: omitted — share parent (sandbox-pod) netns.
			},
			"maskedPaths": []string{
				"/proc/kcore", "/proc/latency_stats",
				"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
				"/sys/firmware",
			},
			"readonlyPaths": []string{
				"/proc/asound", "/proc/bus", "/proc/fs",
				"/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
			},
		},
	}

	out, err := os.Create(filepath.Join(p.BundleDir, "config.json"))
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(spec)
}
