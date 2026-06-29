package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/spec"
)

const (
	composeProject = "hiver"
	// defaultImageName is the logical image used when a create omits the image;
	// it resolves through the config (resolveImage).
	defaultImageName = "agent-base"
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
type DockerRuntime struct {
	// images is the logical-name → ref/pack mapping from HIVE_IMAGES_CONFIG
	// (design §11), plus its file-wide pack default (true unless disabled). A
	// create's image name is resolved through it (resolveImage); an unmapped name
	// is treated as a full ref. When packing, keys of the same image are packed
	// into one pack-host container, each created via POST /v1/<key> (design §6,
	// §10); otherwise each key gets its own (single-key pack-host) container.
	images config
	// snapshotHostDir is the host path bind-mounted as /snapshots in sandbox
	// containers. Set from HIVE_SNAPSHOT_DIR env var. When empty, the named
	// docker volume hiver_snapshots is used as a fallback (no-op on macOS Docker
	// Desktop where named volumes don't surface to the host filesystem).
	snapshotHostDir string
}

func newDockerRuntime() *DockerRuntime {
	return &DockerRuntime{
		images:          loadImagesConfig(),
		snapshotHostDir: os.Getenv("HIVE_SNAPSHOT_DIR"),
	}
}

// imageName returns the logical image name (or full ref) from a config, or empty
// when unset.
func imageName(cfg sandboxgen.SandboxConfig) string {
	if cfg.Image != nil {
		return *cfg.Image
	}
	return ""
}

// snapshotVolumeArg returns the docker -v argument to mount the snapshot
// directory at /snapshots in a sandbox container.
func (r *DockerRuntime) snapshotVolumeArg() string {
	if r.snapshotHostDir != "" {
		return r.snapshotHostDir + ":/snapshots"
	}
	return composeProject + "_snapshots:/snapshots"
}

// labelImageHash records which image a pack pod hosts, so getOrCreate can find an
// existing pod for the image.
const labelImageHash = "hiver.image.hash"

// podNameForImage is the (atomic) name of the pack pod hosting image.
func podNameForImage(image string) string {
	return "hiver-pod-" + shortHash(image)
}

// shortHash is a stable 12-hex-char digest of s, for image-keyed names/labels.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

func (r *DockerRuntime) Lookup(ctx context.Context, key string) (bool, gen.Sandbox, error) {
	// A single-container (non-pack) sandbox has a per-key container we can resolve
	// here. A packed sandbox has none — placement is by image, not a per-key
	// container — so this returns not-running and Start resolves-or-creates the
	// pod and POSTs the key (both idempotent).
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
	// Enumerate both placements: packed sandboxes live inside per-image pods (asked
	// via their supervisor), single-container sandboxes are their own containers
	// (found by label). The two sets never overlap — a pack pod carries no
	// per-key label — so combining can't double-count. This covers a mixed
	// deployment where some images pack and others don't (per-image pack flag).
	packed, err := r.listPacked(ctx)
	if err != nil {
		return nil, err
	}
	labeled, err := r.listLabeled(ctx)
	if err != nil {
		return nil, err
	}
	return append(packed, labeled...), nil
}

// listLabeled enumerates single-container (non-pack) sandboxes by the per-key
// label `docker ps` can see.
func (r *DockerRuntime) listLabeled(ctx context.Context) ([]gen.Sandbox, error) {
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

// listPacked enumerates the sandboxes in pack mode. Packed sandboxes are not
// their own containers (they live inside a per-image pod), so `docker ps` can't
// see them; instead, find each pack pod (by its image-hash label) and ask its
// supervisor (GET /v1) for the keys it currently hosts. Every key maps to the
// pod's routing id, mirroring how startPacked returns the pod id per key.
func (r *DockerRuntime) listPacked(ctx context.Context) ([]gen.Sandbox, error) {
	ids, err := r.packPodIDs(ctx)
	if err != nil {
		return nil, err
	}
	// Non-nil so an empty result serializes as [] (JSON null breaks clients that
	// .map the response).
	sandboxes := []gen.Sandbox{}
	for _, podID := range ids {
		id, err := uuid.Parse(podID)
		if err != nil {
			continue
		}
		summaries, err := podSandboxes(ctx, containerNameFor(podID))
		if err != nil {
			// A pod that's still booting (or briefly unreachable) shouldn't drop the
			// whole listing — skip it and surface the rest.
			log.Printf("controller: list pack pod %s: %v", podID, err)
			continue
		}
		for _, s := range summaries {
			status := gen.SandboxStatus(s.Status)
			sandboxes = append(sandboxes, gen.Sandbox{Id: id, Key: s.Key, Status: &status})
		}
	}
	return sandboxes, nil
}

// podSandboxes asks a pack pod's supervisor (GET /v1) for the sandboxes it hosts
// and their lifecycle status.
func podSandboxes(ctx context.Context, host string) ([]sandboxgen.SandboxSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s:%d/v1", host, sandboxdPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1: status %d", resp.StatusCode)
	}
	var list sandboxgen.SandboxList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode sandbox list: %w", err)
	}
	return list.Sandboxes, nil
}

func (r *DockerRuntime) Start(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	// Resolve the logical image name (or full ref) to the Docker ref to run and
	// whether to pack it (design §11). cfg.Image is intentionally NOT overwritten
	// so the logical name the caller sent ("python") is preserved in the spec
	// sandboxd receives and in any stored/returned config.
	ref, pack := resolveImage(r.images, imageName(cfg))
	if pack {
		return r.startPacked(ctx, key, cfg, ref)
	}
	id := uuid.New()
	containerName := containerNameFor(key)
	// Clear any lingering container of the same name (e.g. one that exited
	// but wasn't auto-removed) so `docker create --name` below doesn't fail
	// with a name conflict. No-op if nothing matches.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	image := ref

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
	// This is a single-key pack pod: the sandbox gets its own veth and its egress
	// is DNAT'd to the host-loopback proxy, so the pod bridge needs ip_forward and
	// route_localnet (mirrors createPackPod). The pod's /proc/sys is read-only, so
	// set them at create time.
	createArgs = append(createArgs,
		"--sysctl", "net.ipv4.ip_forward=1",
		"--sysctl", "net.ipv4.conf.all.route_localnet=1",
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
		)
	}
	// Forward the gateway URL to the pod (compose sets it to http://gateway:10000
	// on the controller). sandboxd injects it into the workload so an agent inside
	// the sandbox can reach the gateway; an unset value is simply not passed.
	if gw := os.Getenv(spec.EnvGatewayURL); gw != "" {
		createArgs = append(createArgs, "-e", spec.EnvGatewayURL+"="+gw)
	}
	// Mount the snapshot volume so this sandbox can write and restore local
	// snapshots. sandboxd boots as a (single-key) pack host: it parks and brings
	// the sandbox up on the POST /v1/<key> below, which carries the config.
	createArgs = append(createArgs, "-v", r.snapshotVolumeArg())
	createArgs = append(createArgs, image, "--snapshot-dir", "/snapshots")
	createStart := time.Now()
	if out, err := exec.CommandContext(ctx, "docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create %s: %v: %s", image, err, out)
	}
	log.Printf("sandbox %s: container created (image pulled if absent) in %s", containerName, time.Since(createStart).Round(time.Millisecond))

	if out, err := exec.CommandContext(ctx, "docker", "start", containerName).CombinedOutput(); err != nil {
		startErr := withContainerLogs(ctx, fmt.Errorf("docker start %s: %v: %s", image, err, out), containerName)
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, startErr
	}

	// Create the sandbox inside the (now-running) pod via POST /v1/<key>, which
	// also blocks until sandboxd is serving — the config travels in the request
	// body. Leave the started container in place on failure: like the k8s
	// anchor/pod, it's the durable reservation a retry can revive.
	sandboxHost := containerNameFor(id.String())
	if err := packCreateSandbox(ctx, sandboxHost, key, cfg); err != nil {
		return gen.Sandbox{}, withContainerLogs(ctx, fmt.Errorf("create sandbox %s: %w", id, err), containerName)
	}
	log.Printf("sandbox %s: sandboxd ready (total start %s)", containerName, time.Since(createStart).Round(time.Millisecond))

	return gen.Sandbox{Id: id, Key: key}, nil
}

// startPacked resolves-or-creates the pack pod hosting cfg's image, then creates
// the sandbox for key inside it via POST /v1/<key>. All keys of an image share
// one pod (and one routing id), so the returned id is the pod's.
func (r *DockerRuntime) startPacked(ctx context.Context, key string, cfg sandboxgen.SandboxConfig, ref string) (gen.Sandbox, error) {
	image := ref
	podName := podNameForImage(image)

	exists, running, err := containerState(ctx, podName)
	if err != nil {
		return gen.Sandbox{}, err
	}
	var podID string
	if running {
		podID, err = containerLabel(ctx, podName, labelSandboxID)
		if err != nil {
			return gen.Sandbox{}, err
		}
	} else {
		if exists {
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", podName).Run()
		}
		id := uuid.New()
		podID = id.String()
		if err := r.createPackPod(ctx, podName, id, image); err != nil {
			return gen.Sandbox{}, err
		}
	}

	podHost := containerNameFor(podID)
	if err := packCreateSandbox(ctx, podHost, key, cfg); err != nil {
		return gen.Sandbox{}, withContainerLogs(ctx, fmt.Errorf("pack %q into pod %s: %w", key, podName, err), podName)
	}
	pid, err := uuid.Parse(podID)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("parse pod id %q: %w", podID, err)
	}
	log.Printf("sandbox %q: packed into pod %s (image %s)", key, podName, image)
	return gen.Sandbox{Id: pid, Key: key}, nil
}

// createPackPod creates+starts a sandboxd pack host for image and waits for
// its API to listen. The pod bridge needs ip_forward + route_localnet so the
// host REDIRECT can funnel packed-sandbox egress to sbxproxy.
func (r *DockerRuntime) createPackPod(ctx context.Context, podName string, id uuid.UUID, image string) error {
	createArgs := []string{
		"create",
		"--name", podName,
		"--label", "com.docker.compose.project=" + composeProject,
		"--label", "com.docker.compose.service=pod-" + shortHash(image),
		"--label", labelImageHash + "=" + shortHash(image),
		"--label", labelSandboxID + "=" + id.String(),
		"--network", composeProject + "_default",
		"--network-alias", containerNameFor(id.String()),
		"--device", "/dev/fuse",
		"--cap-add", "SYS_ADMIN",
		"--cap-add", "NET_ADMIN",
		"--cap-add", "DAC_READ_SEARCH",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
		"--cgroupns", "host",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"--sysctl", "net.ipv4.ip_forward=1",
		"--sysctl", "net.ipv4.conf.all.route_localnet=1",
	}
	if hostHasKVM() {
		createArgs = append(createArgs,
			"--device", "/dev/kvm",
			"--device", "/dev/net/tun",
			"--device-cgroup-rule", "b 7:* rmw",
			"--device-cgroup-rule", "c 10:237 rmw",
		)
	}
	// Mount the same snapshot volume as single-sandbox containers so packed
	// sandboxes can write and restore local snapshots. The volume is shared
	// across all pods; snapshot keys are unique per-sandbox so they don't collide.
	createArgs = append(createArgs, "-v", r.snapshotVolumeArg())
	// Forward the gateway URL so packed sandboxes can reach the gateway, same as
	// single-sandbox containers above.
	if gw := os.Getenv(spec.EnvGatewayURL); gw != "" {
		createArgs = append(createArgs, "-e", spec.EnvGatewayURL+"="+gw)
	}
	// --snapshot-dir overrides the image's default `--help` CMD so sandboxd boots
	// as a pod host with no boot workload.
	createArgs = append(createArgs, image, "--snapshot-dir", "/snapshots")
	if out, err := exec.CommandContext(ctx, "docker", createArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker create pod %s: %v: %s", image, err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "start", podName).CombinedOutput(); err != nil {
		startErr := withContainerLogs(ctx, fmt.Errorf("docker start %s: %v: %s", podName, err, out), podName)
		_ = exec.Command("docker", "rm", "-f", podName).Run()
		return startErr
	}
	return nil
}

// postSandboxOnce makes one POST /v1/<key> attempt against host. done is true on
// a 2xx (the keyed sandbox is up and serving). retry is true when the host is
// only transiently unavailable — a connect/transport error (sandboxd not
// listening yet, or the host gone) or a 502/503 — so the caller may try again or
// move to another host. A non-nil err with retry=false is a definitive rejection
// from sandboxd (a bad config another host would reject identically), carrying
// sandboxd's own error message. Note that once connected, this blocks until
// sandboxd finishes bringing the keyed sandbox up: the connect fails fast, the
// legitimate creation work does not.
func postSandboxOnce(ctx context.Context, host, key string, body []byte) (done, retry bool, err error) {
	url := fmt.Sprintf("http://%s:%d/v1/%s", host, sandboxdPort, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return false, true, err // sandboxd not reachable: retriable
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		drainAndClose(resp)
		return true, false, nil
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		drainAndClose(resp)
		return false, true, fmt.Errorf("POST /v1/%s: status %d", key, resp.StatusCode)
	default:
		// Surface sandboxd's error body (its real failure reason) rather than a
		// bare status code, so the controller's error names the actual cause.
		return false, false, fmt.Errorf("POST /v1/%s: status %d: %s", key, resp.StatusCode, sandboxErrorBody(resp))
	}
}

// packCreateSandbox POSTs the config to /v1/<key> on the pack pod, which brings
// the keyed sandbox up and blocks until it is serving. The connect is retried up
// to sandboxReadyTimeout because the pod was just created and sandboxd may not
// have bound its port yet.
func packCreateSandbox(ctx context.Context, host, key string, cfg sandboxgen.SandboxConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxReadyTimeout)
	defer cancel()
	for {
		done, retry, err := postSandboxOnce(ctx, host, key, body)
		if done {
			return nil
		}
		if !retry {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pod did not accept POST /v1/%s within %s", key, sandboxReadyTimeout)
		case <-time.After(readyProbeInterval):
		}
	}
}

// sandboxErrorBody reads and closes resp.Body and extracts a human-readable
// message: the JSON {"error": ...} sandboxd returns, falling back to the raw
// (trimmed, size-capped) body. Used to surface the sandbox-side failure reason
// in the controller's error instead of a bare status code.
func sandboxErrorBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
	var e sandboxgen.Error
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(b))
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

// Events merges the lifecycle streams of both placements: packed sandboxes
// (each per-image pod's GET /v1/events SSE, via eventsPacked) and
// single-container sandboxes (the docker events label stream, via
// eventsLabeled). Both run unconditionally so a mixed deployment surfaces
// everything; the streams are disjoint (pack pods carry no per-key label).
func (r *DockerRuntime) Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
	packed := r.eventsPacked(ctx)
	labeled, err := r.eventsLabeled(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan gen.SandboxLifecycleEvent, 64)
	go func() {
		defer close(out)
		var wg sync.WaitGroup
		wg.Add(2)
		forward := func(in <-chan gen.SandboxLifecycleEvent) {
			defer wg.Done()
			for ev := range in {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
		go forward(packed)
		go forward(labeled)
		wg.Wait()
	}()
	return out, nil
}

// eventsLabeled streams lifecycle transitions of single-container (non-pack)
// sandboxes from `docker events`, filtered to containers carrying the per-key
// sandbox label.
func (r *DockerRuntime) eventsLabeled(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
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

// packPodIDs returns the routing id of every running pack pod (label-keyed by
// image hash). Shared by listPacked and eventsPacked.
func (r *DockerRuntime) packPodIDs(ctx context.Context) ([]string, error) {
	format := `{{.Label "` + labelSandboxID + `"}}`
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label="+labelImageHash, "--format", format).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// eventsPacked aggregates the lifecycle streams of every pack pod. It holds a
// persistent GET /v1/events connection to each pod (re-discovering pods so new
// ones are picked up and gone ones dropped) and maps each inner PodEvent to a
// SandboxLifecycleEvent keyed by the pod's routing id.
func (r *DockerRuntime) eventsPacked(ctx context.Context) <-chan gen.SandboxLifecycleEvent {
	out := make(chan gen.SandboxLifecycleEvent, 64)
	go func() {
		defer close(out)
		conns := map[string]context.CancelCauseFunc{} // podID → cancel its stream
		defer func() {
			for _, cancel := range conns {
				cancel(nil) // consumer left: don't mark sandboxes destroyed
			}
		}()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			ids, err := r.packPodIDs(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("controller: events: discover pods: %v", err)
			}
			seen := make(map[string]bool, len(ids))
			for _, podID := range ids {
				seen[podID] = true
				if _, ok := conns[podID]; ok {
					continue
				}
				id, err := uuid.Parse(podID)
				if err != nil {
					continue
				}
				cctx, cancel := context.WithCancelCause(ctx)
				conns[podID] = cancel
				go streamPodEvents(cctx, ctx, containerNameFor(podID), id, out)
			}
			for podID, cancel := range conns {
				if !seen[podID] {
					cancel(errPackPodGone) // pod died: streamPodEvents destroys its sandboxes
					delete(conns, podID)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out
}

// errPackPodGone is the cancel cause eventsPacked uses when a pack pod leaves
// discovery (the pod was deleted/terminated). streamPodEvents distinguishes it
// from a plain context cancel (the client disconnecting the events stream) so it
// only marks a pod's sandboxes destroyed when the *pod* died, not when the
// consumer left.
var errPackPodGone = errors.New("pack pod left discovery")

// podStreamDeadAfter is how long a pod's event stream may stay unreconnectable
// (repeated connect failures) before the pod is presumed dead and its sandboxes
// marked destroyed. A transient drop reconnects well within this; a pod that's
// gone for this long isn't coming back on the same address.
const podStreamDeadAfter = 30 * time.Second

// streamPodEvents holds one pod's GET /v1/events SSE open, forwarding each
// PodEvent to out as a SandboxLifecycleEvent and tracking which sandboxes the pod
// hosts. It reconnects across transient drops, but if the pod becomes
// unreachable (can't reconnect for podStreamDeadAfter) or leaves discovery
// (streamCtx cancelled with errPackPodGone), the pod is presumed dead and every
// sandbox it still hosted is emitted as Destroy — otherwise a crashed/OOM'd pod
// would strand its sandboxes "running" forever. A plain streamCtx cancel (the
// consumer disconnecting) leaves the sandboxes alone. sendCtx (the parent events
// context) gates the Destroy emission so it still delivers after streamCtx is
// cancelled by a discovery drop.
func streamPodEvents(streamCtx, sendCtx context.Context, host string, id uuid.UUID, out chan<- gen.SandboxLifecycleEvent) {
	url := fmt.Sprintf("http://%s:%d/v1/events", host, sandboxdPort)
	hosted := map[string]bool{} // sandbox keys this pod currently hosts
	// lastID is the id of the most recent event consumed from this pod. It is
	// passed back as ?lastEventId= on reconnect so a transient drop resumes after
	// the last processed event instead of replaying the whole backlog (design §7).
	var lastID string
	var downSince time.Time
	for streamCtx.Err() == nil {
		err := readPodEventStream(streamCtx, url, id, out, hosted, &lastID)
		if streamCtx.Err() != nil {
			break
		}
		if err == nil {
			downSince = time.Time{} // was connected; a clean drop, not a dead pod
		} else {
			log.Printf("controller: pod %s event stream: %v", id, err)
			if downSince.IsZero() {
				downSince = time.Now()
			}
			if time.Since(downSince) >= podStreamDeadAfter {
				log.Printf("controller: pod %s unreachable for %s; marking %d sandbox(es) destroyed", id, podStreamDeadAfter, len(hosted))
				emitPodDestroyed(sendCtx, id, hosted, out)
				return
			}
		}
		select {
		case <-streamCtx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	// streamCtx cancelled: a discovery drop (errPackPodGone) means the pod died —
	// destroy its sandboxes; a plain cancel (consumer left) leaves them.
	if errors.Is(context.Cause(streamCtx), errPackPodGone) {
		emitPodDestroyed(sendCtx, id, hosted, out)
	}
}

// emitPodDestroyed marks every sandbox a dead pod hosted as Destroy. Gated on ctx
// (the parent events context) so it stops if the consumer has also gone away.
func emitPodDestroyed(ctx context.Context, id uuid.UUID, hosted map[string]bool, out chan<- gen.SandboxLifecycleEvent) {
	for key := range hosted {
		select {
		case out <- gen.SandboxLifecycleEvent{Id: id, Key: key, Status: gen.Destroy}:
		case <-ctx.Done():
			return
		}
	}
}

// readPodEventStream reads one SSE connection until it ends or ctx is cancelled,
// forwarding events and maintaining hosted (the pod's live sandbox set): a key is
// added when it starts and dropped when it is destroyed, so on pod death the
// remaining keys are exactly the sandboxes still believed running.
func readPodEventStream(ctx context.Context, url string, id uuid.UUID, out chan<- gen.SandboxLifecycleEvent, hosted map[string]bool, lastID *string) error {
	// Resume after the last event we processed (set on a prior connection) so a
	// reconnect doesn't replay the backlog. sandboxd serves the monotonic event id
	// on the SSE `id:` line and accepts it back as ?lastEventId=.
	reqURL := url
	if *lastID != "" {
		reqURL = url + "?lastEventId=" + neturl.QueryEscape(*lastID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/events: status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		// Track the SSE event id so a reconnect can resume past it.
		if evID, ok := strings.CutPrefix(line, "id: "); ok {
			*lastID = evID
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var pe sandboxgen.PodEvent
		if err := json.Unmarshal([]byte(data), &pe); err != nil {
			continue
		}
		status, ok := mapPodStatus(pe.Status)
		if !ok {
			continue // no controller-side equivalent (e.g. running)
		}
		switch status {
		case gen.Start:
			hosted[pe.Key] = true
		case gen.Destroy:
			delete(hosted, pe.Key)
		}
		select {
		case out <- gen.SandboxLifecycleEvent{Id: id, Key: pe.Key, Status: status}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

// mapPodStatus maps an inner-sandbox PodEvent status to the controller's
// lifecycle vocabulary (start/stop/die/destroy). starting and running both
// signal "up" → Start (idempotent for consumers); a snapshot of an existing
// sandbox arrives as running, so it must map to Start too.
func mapPodStatus(s sandboxgen.PodEventStatus) (gen.SandboxLifecycleEventStatus, bool) {
	switch s {
	case sandboxgen.PodEventStatusStarting, sandboxgen.PodEventStatusRunning:
		return gen.Start, true
	case sandboxgen.PodEventStatusStopping:
		return gen.Stop, true
	case sandboxgen.PodEventStatusStopped:
		return gen.Destroy, true
	default:
		return "", false
	}
}
