package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gen "github.com/blasten/hive/internal/api/gen/controller"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/spec"
)

const (
	composeProject      = "hive"
	defaultSandboxImage = "hiveruntime/agent-cli:latest"
	labelSandboxID      = "hive.sandbox.id"
)

// DockerRuntime implements SandboxRuntime using local Docker commands.
type DockerRuntime struct{}

func newDockerRuntime() *DockerRuntime {
	return &DockerRuntime{}
}

func (r *DockerRuntime) Lookup(id string) (bool, gen.Sandbox, error) {
	name := containerNameFor(id)
	_, running, err := containerState(name)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	if !running {
		return false, gen.Sandbox{}, nil
	}
	sb := gen.Sandbox{Id: id}
	return true, sb, nil
}

func (r *DockerRuntime) List() ([]gen.Sandbox, error) {
	out, err := exec.Command("docker", "ps", "--filter", "label="+labelSandboxID, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	prefix := composeProject + "-sandbox-"
	sandboxes := make([]gen.Sandbox, 0, len(names))
	for _, name := range names {
		id := strings.TrimPrefix(name, prefix)
		sandboxes = append(sandboxes, gen.Sandbox{Id: id})
	}
	return sandboxes, nil
}

func (r *DockerRuntime) Start(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	specBytes, err := json.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}
	specPath := filepath.Join(os.TempDir(), "hive-spec-"+id+".yaml")
	if err := os.WriteFile(specPath, specBytes, 0o644); err != nil {
		return gen.Sandbox{}, fmt.Errorf("write spec: %w", err)
	}
	defer os.Remove(specPath)

	containerName := containerNameFor(id)
	// Clear any lingering container of the same name (e.g. one that exited
	// but wasn't auto-removed) so `docker create --name` below doesn't fail
	// with a name conflict. No-op if nothing matches.
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	var image = defaultSandboxImage
	if cfg.Image != nil && *cfg.Image != "" {
		image = *cfg.Image
	}

	serviceLabel := "sandbox-" + id
	createArgs := []string{
		"create",
		"--name", containerName,
		"--label", "com.docker.compose.project=" + composeProject,
		"--label", "com.docker.compose.service=" + serviceLabel,
		"--label", labelSandboxID + "=" + id,
		"--network", composeProject + "_default",
		"--device", "/dev/fuse",
		// The caps runc needs to set up the inner container (MKNOD, SYS_CHROOT,
		// SET*, FOWNER, CHOWN) are already in Docker's default set; only the three
		// below go beyond it.
		//
		// SYS_ADMIN: the catch-all "do privileged things" cap. Enables the mount()
		// syscall and namespace creation, e.g. mounting the FUSE filesystem on
		// /dev/fuse, the overlayfs that backs each sandbox rootfs, and unsharing
		// the mount/pid namespaces runc and firecracker set up.
		"--cap-add", "SYS_ADMIN",
		// NET_ADMIN: network configuration inside the container, e.g. creating the
		// firecracker tap device, bringing it up, and installing the iptables DNAT
		// rule that redirects guest egress to the host-loopback proxy.
		"--cap-add", "NET_ADMIN",
		// DAC_READ_SEARCH: bypass file read + directory-execute permission checks,
		// e.g. traversing and reading snapshot/overlay trees whose dirs are owned
		// by other UIDs (DAC_OVERRIDE, a Docker default, covers write; this covers
		// read/search).
		"--cap-add", "DAC_READ_SEARCH",
		// apparmor=unconfined: the default Docker AppArmor profile blocks the
		// mount/umount operations above; unconfined lifts that.
		"--security-opt", "apparmor=unconfined",
		// seccomp=unconfined: the default seccomp profile blocks syscalls the
		// nested runtimes need, e.g. mount(), the loop-device ioctls used for
		// snapshotting, and keyctl(); unconfined allows them.
		"--security-opt", "seccomp=unconfined",
	}
	// Both container (runc) and microvm (firecracker) isolation create cgroup
	// sub-trees, so the host cgroup tree must be writable and the cgroup
	// namespace shared with the host.
	createArgs = append(createArgs,
		"--cgroupns", "host",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
	)
	// The microvm backend additionally needs /dev/kvm (the VMM) and
	// /dev/net/tun (the tap device that carries guest egress). It also
	// loop-mounts the guest's overlay image on the host to capture/restore
	// snapshots; a container's /dev has no loop nodes and its device cgroup
	// denies them, so grant the loop block major (7) and the loop-control char
	// device (10:237). sandboxd mknod's the nodes on demand under these rules.
	if cfg.Isolation != nil && *cfg.Isolation == sandboxgen.Microvm {
		createArgs = append(createArgs,
			"--device", "/dev/kvm",
			"--device", "/dev/net/tun",
			"--device-cgroup-rule", "b 7:* rmw",
			"--device-cgroup-rule", "c 10:237 rmw",
			// Guest egress is DNAT'd to the host-loopback proxy on the tap; the
			// kernel otherwise drops those loopback-destined forwarded packets as
			// martians. route_localnet lifts that, but the pod's /proc/sys is
			// read-only, so set it at create time. The kernel ORs the per-device
			// and "all" values, so "all" covers the runtime-created tap.
			"--sysctl", "net.ipv4.conf.all.route_localnet=1",
		)
	}
	if cfg.Env != nil {
		for k, v := range *cfg.Env {
			createArgs = append(createArgs, "-e", k+"="+v)
		}
	}

	// Mount volumes
	for _, fs := range cfg.Fs {
		local, err := fs.AsLocalFileSystem()
		if err != nil || local.Origin == nil {
			continue
		}
		createArgs = append(createArgs, "-v", *local.Origin+":"+local.Mount+spec.BackendSuffix)
	}

	createArgs = append(createArgs, "-v", composeProject+"_snapshots:/snapshots")

	createArgs = append(createArgs,
		image,
		"--spec", "/mnt/spec.json",
		"--snapshot-dir", "/snapshots",
	)
	if out, err := exec.Command("docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create %s: %v: %s", image, err, out)
	}

	if out, err := exec.Command("docker", "cp", specPath, containerName+":/mnt/spec.json").CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker cp spec: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "start", containerName).CombinedOutput(); err != nil {
		startErr := withContainerLogs(fmt.Errorf("docker start %s: %v: %s", image, err, out), containerName)
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, startErr
	}

	return gen.Sandbox{Id: id}, nil
}

func (r *DockerRuntime) Shutdown(id string) error {
	name := containerNameFor(id)
	exists, running, err := containerState(name)
	if err != nil {
		return err
	}
	if !exists {
		return ErrSandboxNotFound
	}
	if running {
		// Allow 60 s for sandboxd to capture the snapshot before SIGKILL.
		// `-t` (not `--timeout`) — the Debian Bookworm `docker.io` package
		// ships CLI 20.10, which predates the `--timeout` long form.
		if out, err := exec.Command("docker", "stop", "-t", "60", name).CombinedOutput(); err != nil {
			return fmt.Errorf("docker stop %s: %v: %s", name, err, out)
		}
	}
	if out, err := exec.Command("docker", "rm", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm %s: %v: %s", name, err, out)
	}
	return nil
}

func containerNameFor(id string) string {
	return composeProject + "-sandbox-" + id
}

// containerState returns whether the named container exists and whether it is
// running. A missing container is (false, false, nil) rather than an error.
func containerState(name string) (exists, running bool, err error) {
	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "No such object") {
			return false, false, nil
		}
		return false, false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	return true, strings.TrimSpace(string(out)) == "true", nil
}

// containerLogs returns recent log output for a container, or empty string if
// unavailable (e.g. the container was already removed).
func containerLogs(name string) string {
	out, _ := exec.Command("docker", "logs", "--tail", "100", name).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// withContainerLogs appends the container's recent logs to err, if any exist.
func withContainerLogs(err error, container string) error {
	logs := containerLogs(container)
	return fmt.Errorf("%w\n\ncontainer logs:\n%s\n", err, logs)
}

type dockerRawEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (r *DockerRuntime) Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
	cmd := exec.CommandContext(ctx, "docker", "events",
		"--filter", "label="+labelSandboxID,
		"--filter", "type=container",
		"--format", "{{json .}}")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker events pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker events start: %w", err)
	}
	ch := make(chan gen.SandboxLifecycleEvent, 16)
	go func() {
		defer close(ch)
		defer cmd.Wait() //nolint:errcheck
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var e dockerRawEvent
			if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
				continue
			}
			var status gen.SandboxLifecycleEventStatus
			switch e.Action {
			case "start":
				status = gen.Start
			case "stop":
				status = gen.Stop
			case "die":
				status = gen.Die
			case "destroy":
				status = gen.Destroy
			default:
				continue
			}
			id := e.Actor.Attributes[labelSandboxID]
			if id == "" {
				continue
			}
			select {
			case ch <- gen.SandboxLifecycleEvent{Id: id, Status: status}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
