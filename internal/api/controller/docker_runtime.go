package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/spec"
)

const (
	composeProject      = "hiver"
	defaultSandboxImage = "hiversh/agent-cli:latest"
	// labelSandboxKey holds the caller-chosen key; labelSandboxID holds the
	// server-assigned uuid. The container name is derived from the key, so
	// idempotent lookups resolve by key while the uuid travels as a label.
	labelSandboxKey = "hiver.sandbox.key"
	labelSandboxID  = "hiver.sandbox.id"
)

// hostHasKVM reports whether the Docker host exposes /dev/kvm, the device a
// microvm image needs. The runtime grants the microvm devices to every sandbox
// container when it's present (see Start): isolation is derived from the image
// by sandboxd, not declared in the request, so the runtime can't tell a microvm
// image from a container image up front. Passing `--device /dev/kvm` when the
// host lacks it would fail docker create, so the grant is gated on its presence.
func hostHasKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// DockerRuntime implements SandboxRuntime using local Docker commands.
type DockerRuntime struct{}

func newDockerRuntime() *DockerRuntime {
	return &DockerRuntime{}
}

func (r *DockerRuntime) Lookup(ctx context.Context, key string) (bool, gen.Sandbox, error) {
	name := containerNameFor(key)
	_, running, err := containerState(ctx, name)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	if !running {
		return false, gen.Sandbox{}, nil
	}
	idStr, err := containerLabel(ctx, name, labelSandboxID)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return false, gen.Sandbox{}, fmt.Errorf("parse sandbox id label %q: %w", idStr, err)
	}
	return true, gen.Sandbox{Id: id, Key: key}, nil
}

func (r *DockerRuntime) List(ctx context.Context) ([]gen.Sandbox, error) {
	format := `{{.Label "` + labelSandboxKey + `"}} {{.Label "` + labelSandboxID + `"}}`
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label="+labelSandboxKey, "--format", format).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sandboxes := make([]gen.Sandbox, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		id, err := uuid.Parse(fields[1])
		if err != nil {
			continue
		}
		sandboxes = append(sandboxes, gen.Sandbox{Id: id, Key: fields[0]})
	}
	return sandboxes, nil
}

func (r *DockerRuntime) Start(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	id := uuid.New()
	specBytes, err := json.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}

	containerName := containerNameFor(key)
	// Clear any lingering container of the same name (e.g. one that exited
	// but wasn't auto-removed) so `docker create --name` below doesn't fail
	// with a name conflict. No-op if nothing matches.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	var image = defaultSandboxImage
	if cfg.Image != nil && *cfg.Image != "" {
		image = *cfg.Image
	}

	serviceLabel := "sandbox-" + key
	createArgs := []string{
		"create",
		"--name", containerName,
		"--label", "com.docker.compose.project=" + composeProject,
		"--label", "com.docker.compose.service=" + serviceLabel,
		"--label", labelSandboxKey + "=" + key,
		"--label", labelSandboxID + "=" + id.String(),
		"--network", composeProject + "_default",
		// The container keeps its key-derived --name (the atomic per-key lock),
		// but the gateway routes by id, so expose an id-named DNS alias on the
		// shared network for it to resolve as hiver-sandbox-<id>.
		"--network-alias", containerNameFor(id.String()),
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
	// IPv6 egress is blocked inside the pod by sandboxd (ip6tables in the
	// isolation backends' RedirectEgress), not here: it needs only CAP_NET_ADMIN,
	// so it works identically under docker and k8s and keeps the control next to
	// the v4 egress rules it mirrors.
	//
	// A microvm image additionally needs /dev/kvm (the VMM) and /dev/net/tun
	// (the tap device that carries guest egress). It also loop-mounts the guest's
	// overlay image on the host to capture/restore snapshots; a container's /dev
	// has no loop nodes and its device cgroup denies them, so grant the loop block
	// major (7) and the loop-control char device (10:237). sandboxd mknod's the
	// nodes on demand under these rules.
	//
	// Isolation is no longer a config field — sandboxd derives it from the image
	// at boot — so the runtime can't know in advance whether this image is a
	// microvm image. Instead, grant the microvm devices whenever the host exposes
	// /dev/kvm: harmless for a container image (it simply ignores them), and the
	// prerequisite for a microvm image to boot at all. On a host without KVM these
	// are skipped (a `--device /dev/kvm` would otherwise fail docker create), and
	// a microvm image then fails with sandboxd's friendly KVM error.
	if hostHasKVM() {
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
	// The spec is delivered as JSON in an env var rather than a mounted file;
	// sandboxd reads it via spec.LoadEnv.
	createArgs = append(createArgs, "-e", spec.EnvSpec+"="+string(specBytes))
	if cfg.ExtraHosts != nil {
		for _, h := range *cfg.ExtraHosts {
			createArgs = append(createArgs, "--add-host", h)
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

	// Provision the local snapshot volume only when snapshots aren't routed to a
	// FUSE drive (snapshot.mount); otherwise it's unnecessary and would collide
	// with a FUSE mount at the same path.
	if !usesSnapshotMount(cfg) {
		createArgs = append(createArgs, "-v", composeProject+"_snapshots:/snapshots")
	}

	// Always pass --snapshot-dir so the container overrides the image's default
	// `--help` CMD and sandboxd boots; the dir is empty (local snapshots
	// disabled) when snapshots route to a FUSE drive instead.
	snapDir := "/snapshots"
	if usesSnapshotMount(cfg) {
		snapDir = ""
	}
	createArgs = append(createArgs, image, "--snapshot-dir", snapDir)
	createStart := time.Now()
	if out, err := exec.CommandContext(ctx, "docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create %s: %v: %s", image, err, out)
	}
	log.Printf("sandbox %s: container created (image pulled if absent) in %s", containerName, time.Since(createStart).Round(time.Millisecond))

	startStart := time.Now()
	if out, err := exec.CommandContext(ctx, "docker", "start", containerName).CombinedOutput(); err != nil {
		startErr := withContainerLogs(ctx, fmt.Errorf("docker start %s: %v: %s", image, err, out), containerName)
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, startErr
	}
	log.Printf("sandbox %s: container started in %s", containerName, time.Since(startStart).Round(time.Millisecond))

	// Block until sandboxd is actually serving, mirroring the k8s runtime's
	// wait on the pod readiness probe: the container is started but sandboxd
	// (and the workload) still need to boot, so a returned id isn't reachable
	// yet. Leave the started container in place on failure — like the k8s
	// anchor/pod, it's the durable reservation a retry can revive.
	sandboxHost := containerNameFor(id.String())
	readyStart := time.Now()
	if err := waitSandboxReady(ctx, sandboxHost); err != nil {
		return gen.Sandbox{}, withContainerLogs(ctx, fmt.Errorf("wait sandbox %s ready: %w", id, err), containerName)
	}
	log.Printf("sandbox %s: sandboxd ready in %s (total start %s)", containerName, time.Since(readyStart).Round(time.Millisecond), time.Since(createStart).Round(time.Millisecond))

	return gen.Sandbox{Id: id, Key: key}, nil
}

func (r *DockerRuntime) Shutdown(ctx context.Context, key string) error {
	name := containerNameFor(key)
	exists, running, err := containerState(ctx, name)
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
		if out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "60", name).CombinedOutput(); err != nil {
			return fmt.Errorf("docker stop %s: %v: %s", name, err, out)
		}
	}
	if out, err := exec.CommandContext(ctx, "docker", "rm", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm %s: %v: %s", name, err, out)
	}
	return nil
}

// containerNameFor builds the hiver-sandbox-<segment> DNS name. The segment is
// the key for the docker container --name and the k8s anchor ConfigMap (the
// per-key idempotency lock), and the id for the k8s Pod/Service and the docker
// network alias the gateway routes to.
func containerNameFor(segment string) string {
	return composeProject + "-sandbox-" + segment
}

// containerLabel returns the value of a single label on the named container.
func containerLabel(ctx context.Context, name, label string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", `{{index .Config.Labels "`+label+`"}}`, name).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s label %s: %w", name, label, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// containerState returns whether the named container exists and whether it is
// running. A missing container is (false, false, nil) rather than an error.
func containerState(ctx context.Context, name string) (exists, running bool, err error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
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
func containerLogs(ctx context.Context, name string) string {
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "100", name).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// withContainerLogs appends the container's recent logs to err, if any exist.
func withContainerLogs(ctx context.Context, err error, container string) error {
	logs := containerLogs(ctx, container)
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
		"--filter", "label="+labelSandboxKey,
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
			key := e.Actor.Attributes[labelSandboxKey]
			idStr := e.Actor.Attributes[labelSandboxID]
			if key == "" || idStr == "" {
				continue
			}
			id, err := uuid.Parse(idStr)
			if err != nil {
				continue
			}
			select {
			case ch <- gen.SandboxLifecycleEvent{Id: id, Key: key, Status: status}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
