package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// BundleParams configure the OCI runtime spec sandboxd writes for the
// agent container. The bundle layout matches what `runc run -b <dir>`
// expects: <dir>/config.json + <dir>/<RootPath>/.
type BundleParams struct {
	BundleDir   string            // bundle root — must already contain rootfs/
	RootPath    string            // rootfs dir relative to BundleDir; defaults to "rootfs"
	ImageConfig *ImageConfig      // image-supplied entrypoint/cmd/env/cwd
	ExtraEnv    map[string]string // sandboxd-injected KEY=VAL entries
	Mounts      []BindMount       // additional host bind mounts (e.g. /workspace)
	Hostname    string
	// CgroupsPath, if non-empty, is set as the container's cgroupsPath in the
	// OCI spec and a poststop hook is added to drain the cgroup before runc
	// destroys it.  This eliminates the "container's cgroup is not empty"
	// warning that appears when exec'd processes haven't been fully reaped by
	// the time runc runs its cleanup.
	CgroupsPath string
	// VcpuCount and MemoryMiB, when > 0, are written into the bundle's
	// linux.resources to cap the agent's CPU and memory. CPU is enforced as a
	// quota over the standard 100ms period (VcpuCount full cores); memory as a
	// hard cgroup limit in bytes.
	VcpuCount int
	MemoryMiB int
	// Terminal sets the OCI process.terminal flag. When true, `runc run`
	// allocates a pty for the entrypoint and proxies it through runc's own
	// stdio (which the caller must supply as a pty slave), giving the
	// entrypoint a controlling terminal. Used to back the sandbox's tty option.
	Terminal bool
	// ReadyFifo, if non-empty, installs a poststart hook that writes a byte to
	// this fifo once the entrypoint is running, so the caller can block on a
	// read instead of polling `runc state`. The path is interpreted in the
	// runtime (host) mount namespace where runc executes hooks.
	ReadyFifo string
	// NetnsPath, if non-empty, joins the container to an existing network
	// namespace at this path (e.g. /var/run/netns/<key>) instead of sharing the
	// sandbox-pod's netns. Used when packing N sandboxes into one pod so each
	// gets a distinct source IP for per-source egress (design §6). Empty keeps
	// the historical shared-netns behavior.
	NetnsPath string
}

// MakeFifo creates a named pipe at path, removing any stale node first. The
// caller is expected to hold a read end open (see container.LaunchAgent) so a
// poststart hook opening the write end never blocks and its byte is buffered.
func MakeFifo(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o600)
}

// cpuQuotaPeriodUs is the CFS scheduling period (microseconds) the CPU quota is
// expressed against; quota = VcpuCount * period yields VcpuCount whole cores.
const cpuQuotaPeriodUs = 100_000

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
	for name, value := range p.ExtraEnv {
		env = append(env, name+"="+value)
	}

	rootPath := p.RootPath
	if rootPath == "" {
		rootPath = "rootfs"
	}

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

	namespaces := []map[string]string{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
		// network: omitted by default — share parent (sandbox-pod) netns.
	}
	if p.NetnsPath != "" {
		// Packed sandbox: join its own netns so it has a distinct source IP.
		namespaces = append(namespaces, map[string]string{"type": "network", "path": p.NetnsPath})
	}
	linux := map[string]any{
		"namespaces": namespaces,
		"maskedPaths": []string{
			"/proc/kcore", "/proc/latency_stats",
			"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
			"/sys/firmware",
		},
		"readonlyPaths": []string{
			"/proc/asound", "/proc/bus", "/proc/fs",
			"/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
		},
	}
	if p.CgroupsPath != "" {
		linux["cgroupsPath"] = p.CgroupsPath
	}
	if resources := bundleResources(p.VcpuCount, p.MemoryMiB); resources != nil {
		linux["resources"] = resources
	}

	spec := map[string]any{
		"ociVersion": "1.0.2-dev",
		"process": map[string]any{
			"terminal": p.Terminal,
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
		"root":     map[string]any{"path": rootPath, "readonly": false},
		"hostname": p.Hostname,
		"mounts":   mounts,
		"linux":    linux,
	}

	// poststart fires after the entrypoint is exec'd (the "running" edge
	// sandboxd otherwise polls for). The hook runs in the runtime namespace,
	// so it writes to the host-side fifo; keep it trivial — a hook that errors
	// makes runc tear the container back down.
	if p.ReadyFifo != "" {
		spec["hooks"] = map[string]any{
			"poststart": []map[string]any{{
				"path": "/bin/sh",
				"args": []string{"sh", "-c", "echo 1 > " + p.ReadyFifo},
			}},
		}
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

// bundleResources builds the OCI linux.resources map capping CPU and memory,
// or nil when neither is set. runc translates these to the host's cgroup
// version, so the same shape works on cgroup v1 and v2.
func bundleResources(vcpuCount, memoryMiB int) map[string]any {
	resources := map[string]any{}
	if vcpuCount > 0 {
		resources["cpu"] = map[string]any{
			"quota":  vcpuCount * cpuQuotaPeriodUs,
			"period": cpuQuotaPeriodUs,
		}
	}
	if memoryMiB > 0 {
		resources["memory"] = map[string]any{
			"limit": int64(memoryMiB) * 1024 * 1024,
		}
	}
	if len(resources) == 0 {
		return nil
	}
	return resources
}
