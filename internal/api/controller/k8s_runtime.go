package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/spec"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	sandboxdPort = 8099

	// sandboxContainerName is the name of the sandbox container in every sandbox
	// and prewarm pod (see sandboxPodSpec). Prewarm discovery resolves the
	// container by this name to read its args and env.
	sandboxContainerName = "sandbox"

	// packArg and prewarmArg are the sandboxd flags that mark a pod as a parked,
	// multi-tenant prewarm host: --pack runs many sandboxes in the one pod, and
	// --prewarm boots sandboxd and waits for the first PUT /v1/config before
	// launching a workload. A pod can serve prewarmed sandboxes for an image only
	// when its container carries BOTH flags, so prewarm discovery requires both.
	packArg    = "--pack"
	prewarmArg = "--prewarm"

	// podIPTimeout bounds how long Start/Lookup wait for the scheduler to assign the
	// pod an IP. This covers scheduling and image pull on a cold node; sandboxd boot
	// is waited for separately by waitSandboxReady (the /v1/ping?block=true call).
	podIPTimeout = 120 * time.Second

	// defaultVcpuCount and defaultMemoryMiB mirror isolation.DefaultVcpuCount /
	// DefaultMemoryMiB: the compute sandboxd boots a sandbox with when the config
	// leaves cpu/memory unset. The pod's resource limits must match what the
	// guest is actually given, so they're resolved with the same defaults.
	defaultVcpuCount = 1.0
	defaultMemoryMiB = 512

	// podMemoryOverheadMiB is added to the guest RAM (config memory) when setting
	// the pod's memory *limit*: the pod also runs firecracker/sandboxd and the
	// sidecars (sbxproxy, sbxfuse) outside the guest, so a limit of exactly the
	// guest size would OOMKill the pod. The request stays at the guest size so the
	// scheduler reserves only what the guest consumes.
	podMemoryOverheadMiB = 256
)

// sandboxResources maps a SandboxConfig's cpu/memory onto the pod container's
// k8s resource requests and limits so the scheduler accounts for what the guest
// actually consumes (without it the pod is best-effort and big-guest microVMs
// overpack a node). CPU request = limit = cpu (the ceiling the guest can burst
// to). Memory request = guest size; the memory limit adds host-side overhead
// for firecracker + sandboxd + sidecars (see podMemoryOverheadMiB).
func sandboxResources(cfg sandboxgen.SandboxConfig) corev1.ResourceRequirements {
	cpuLimit := defaultVcpuCount
	if cfg.Cpu != nil && *cfg.Cpu > 0 {
		cpuLimit = *cfg.Cpu
	}
	cpuReq := cpuLimit
	memMiB := defaultMemoryMiB
	if cfg.Memory != nil && *cfg.Memory > 0 {
		memMiB = *cfg.Memory
	}
	memReq := resource.NewQuantity(int64(memMiB)*1024*1024, resource.BinarySI)
	memLimit := resource.NewQuantity(int64(memMiB+podMemoryOverheadMiB)*1024*1024, resource.BinarySI)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    milliCPU(cpuReq),
			corev1.ResourceMemory: *memReq,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    milliCPU(cpuLimit),
			corev1.ResourceMemory: *memLimit,
		},
	}
}

// milliCPU converts a fractional core count into a k8s CPU quantity in
// millicores, rounding to the nearest milli so float representation error
// (e.g. 1.001*1000 = 1000.999…) doesn't drop a milli.
func milliCPU(cores float64) resource.Quantity {
	return *resource.NewMilliQuantity(int64(math.Round(cores*1000)), resource.DecimalSI)
}

// sandboxHTTPClient talks to sandboxd over the pod network. A keep-alive
// transport lets back-to-back requests to the same pod IP reuse one TCP
// connection, saving a connect+handshake on the latency path. (The callers must
// drain response bodies for the connection to return to the pool.)
var sandboxHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

// drainAndClose empties then closes a response body so its connection is
// returned to sandboxHTTPClient's idle pool for reuse rather than discarded.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// K8sRuntime implements SandboxRuntime using the Kubernetes API.
type K8sRuntime struct {
	client    kubernetes.Interface
	namespace string
	// packs is the in-memory snapshot of prewarm hosts (image → host IPs) the
	// getOrCreate fast path and the events stream read instead of listing pods on
	// every request. A single background poller (startPackCachePoller) refreshes
	// it; see pack_cache.go.
	packs packCache
}

func newK8sRuntime() (*K8sRuntime, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig: %w", err)
		}
	}
	// Raise the client-side rate limiter above the client-go defaults (QPS 5 /
	// Burst 10). A concurrent claim burst issues one pod Update per claim plus the
	// reconciler's periodic writes; at the defaults those throttle (observed as
	// "client-side throttling" delays added to claim latency). Sized via env so it
	// can track the expected claim concurrency without a rebuild.
	config.QPS = envFloat32("HIVE_CLIENT_QPS", 50)
	config.Burst = envInt("HIVE_CLIENT_BURST", 100)

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	ns := os.Getenv("HIVE_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	r := &K8sRuntime{client: client, namespace: ns}
	r.startRecycler(context.Background())
	// The pack cache backs the getOrCreate fast path and the events stream, so
	// start its poller before serving any request.
	r.startPackCachePoller(context.Background())
	return r, nil
}

// envFloat32 reads name as a float32, returning def if unset or unparseable.
func envFloat32(name string, def float32) float32 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 32)
	if err != nil {
		log.Printf("controller: invalid %s=%q, using %v: %v", name, v, def, err)
		return def
	}
	return float32(f)
}

// envInt reads name as an int, returning def if unset or unparseable.
func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("controller: invalid %s=%q, using %d: %v", name, v, def, err)
		return def
	}
	return n
}

// podTerminated reports whether a pod has finished running. A Pending pod is
// still considered alive: it has been created and is coming up, so Lookup/List
// must not treat it as absent (which would make the handler try to recreate it
// and fail on AlreadyExists).
func podTerminated(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

func (r *K8sRuntime) Lookup(ctx context.Context, key string) (bool, gen.Sandbox, error) {
	pod, err := r.podForKey(key)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	if pod == nil || podTerminated(pod.Status.Phase) {
		return false, gen.Sandbox{}, nil
	}
	sb, err := r.describeReadyPod(ctx, key, pod)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	return true, sb, nil
}

// describeReadyPod resolves pod's IP (waiting if it hasn't been assigned yet)
// then confirms sandboxd is serving with a block=true ping — rather than
// handing back an id the gateway can't reach. The returned id encodes the pod
// IP, which is how the gateway routes. waitSandboxReady returns promptly when
// the sandbox is already up, so this is cheap for a live pod. Shared by Lookup
// and Start's "another caller already created this pod" branch.
func (r *K8sRuntime) describeReadyPod(ctx context.Context, key string, pod *corev1.Pod) (gen.Sandbox, error) {
	ip := pod.Status.PodIP
	if ip == "" {
		ready, err := r.waitForPodIP(ctx, pod.Name)
		if err != nil {
			return gen.Sandbox{}, err
		}
		ip = ready.Status.PodIP
	}
	if err := waitSandboxReady(ctx, ip, key); err != nil {
		return gen.Sandbox{}, err
	}
	id, err := ipID(ip)
	if err != nil {
		return gen.Sandbox{}, err
	}
	return gen.Sandbox{Id: id, Key: key}, nil
}

// ipID encodes an IPv4 address into a UUID by packing its four octets into the
// leading bytes (the rest stay zero). The id doubles as the route target: the
// gateway decodes these bytes back to the pod IP and dials it directly, so no
// per-sandbox Service or DNS lookup is needed.
func ipID(ip string) (uuid.UUID, error) {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return uuid.Nil, fmt.Errorf("pod IP %q is not IPv4", ip)
	}
	var u uuid.UUID
	copy(u[:4], v4)
	return u, nil
}

// waitForPodIP blocks until the pod has been assigned an IP (scheduled and on
// the network) then returns the pod, or errors on timeout/termination. The
// caller reads the IP off pod.Status.PodIP and confirms sandboxd is serving with
// a direct block=true ping (waitSandboxReady), so there is no kubelet readiness
// probe on the pod. It watches rather than polls, so the IP is observed the
// instant it's assigned.
func (r *K8sRuntime) waitForPodIP(ctx context.Context, podName string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, podIPTimeout)
	defer cancel()

	sel := fields.OneTermEqualSelector("metadata.name", podName).String()
	for {
		w, err := r.client.CoreV1().Pods(r.namespace).Watch(ctx, metav1.ListOptions{FieldSelector: sel})
		if err != nil {
			return nil, fmt.Errorf("watch pod %s: %w", podName, err)
		}
		pod, err := podIPFromWatch(ctx, w)
		w.Stop()
		switch {
		case err != nil:
			return nil, err
		case pod != nil:
			return pod, nil
		case ctx.Err() != nil:
			return nil, fmt.Errorf("pod %s was not assigned an IP within %s", podName, podIPTimeout)
		}
	}
}

// podIPFromWatch returns the pod once an IP is assigned, an error if the pod
// terminates first, or (nil, nil) if the watch ends before either happens (the
// caller re-watches while time remains).
func podIPFromWatch(ctx context.Context, w watch.Interface) (*corev1.Pod, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil, nil
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if podTerminated(pod.Status.Phase) {
				return nil, fmt.Errorf("pod %s terminated before getting an IP", pod.Name)
			}
			if pod.Status.PodIP != "" {
				return pod, nil
			}
		}
	}
}

// logPodTimings splits the create→IP window into scheduling vs. the rest
// (image/container/CNI) using the pod's own condition timestamps, so a slow
// start can be attributed to the scheduler or the container runtime rather than
// lumped together. Best-effort: it logs whichever signals are populated by the
// time the IP appears (PodScheduled always is; the container's running time only
// once containerd reports it).
func logPodTimings(pod *corev1.Pod) {
	created := pod.CreationTimestamp.Time
	if created.IsZero() {
		return
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			log.Printf("sandbox %s: scheduled %s after create", pod.Name,
				c.LastTransitionTime.Sub(created).Round(time.Millisecond))
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil {
			log.Printf("sandbox %s: container running %s after create (image+create)", pod.Name,
				cs.State.Running.StartedAt.Sub(created).Round(time.Millisecond))
		}
	}
}

// podForKey returns the dedicated per-key Pod carrying key, preferring a live
// one, or nil if the key has no Pod at all. Pods are named by id, so the
// caller-chosen key is resolved through the hiver.sandbox.key label rather than
// the object name. Prewarm host pods are skipped: they carry the label too (with
// their own key) but host many packed sandboxes rather than being one — packed
// sandboxes are resolved by querying the host, not by this label lookup.
func (r *K8sRuntime) podForKey(key string) (*corev1.Pod, error) {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSandboxKey + "=" + key,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods for key %q: %w", key, err)
	}
	var chosen *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if _, isPack := prewarmPodImage(p); isPack {
			continue
		}
		if !podTerminated(p.Status.Phase) {
			return p, nil
		}
		chosen = p
	}
	return chosen, nil
}

func (r *K8sRuntime) List(ctx context.Context) ([]gen.Sandbox, error) {
	// Non-nil so an empty result serializes as [] (a JSON null breaks clients
	// that .map the response).
	sandboxes := []gen.Sandbox{}

	// Sandboxes packed inside prewarm hosts aren't their own Pods, so the label
	// listing below can't see them — ask each host (GET /v1) for the keys it
	// currently hosts and their status, mirroring the docker runtime's listPacked.
	packs, err := r.listPackPods(ctx)
	if err != nil {
		return nil, err
	}
	isPack := make(map[string]bool, len(packs))
	for _, pod := range packs {
		isPack[pod.Name] = true
		ip := pod.Status.PodIP
		if ip == "" {
			continue
		}
		id, err := ipID(ip)
		if err != nil {
			continue
		}
		summaries, err := podSandboxes(ctx, ip)
		if err != nil {
			// A host still booting (or briefly unreachable) shouldn't drop the whole
			// listing — skip it and surface the rest.
			log.Printf("controller: list pack pod %s: %v", pod.Name, err)
			continue
		}
		for _, s := range summaries {
			status := gen.SandboxStatus(s.Status)
			sandboxes = append(sandboxes, gen.Sandbox{Id: id, Key: s.Key, Status: &status})
		}
	}

	// Dedicated per-key (cold-boot) sandbox Pods, found by label. Skip the prewarm
	// hosts: they carry the label too but are hosts, not sandboxes (their packed
	// keys were already enumerated above).
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSandboxKey,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if isPack[pod.Name] {
			continue
		}
		if podTerminated(pod.Status.Phase) || pod.Status.PodIP == "" {
			continue
		}
		id, err := ipID(pod.Status.PodIP)
		if err != nil {
			continue
		}
		sandboxes = append(sandboxes, gen.Sandbox{Id: id, Key: pod.Labels[labelSandboxKey]})
	}
	return sandboxes, nil
}

func (r *K8sRuntime) Start(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	// Prefer a cached prewarm host for this image: if one is standing by, pack the
	// sandbox into it (POST /v1/<key>) straight from the in-memory cache, with no
	// orchestrator round-trip. tryPackCreate reports ok=false when no host is
	// cached for the image, in which case we cold-boot a dedicated per-key Pod.
	if sb, ok, err := r.tryPackCreate(ctx, key, cfg); err != nil || ok {
		return sb, err
	}

	// No prewarm host for this image — cold-boot a dedicated per-key Pod.
	// The Pod is created on a detached context so a client that disconnects
	// mid-request still leaves a running sandbox behind; only the readiness wait
	// in bootPod honors ctx.
	createCtx := context.Background()

	// The Pod is named by key, which makes Create the atomic per-key arbiter:
	// the API server rejects all but the first caller with AlreadyExists, so two
	// controller replicas racing the same key can't both boot a sandbox. The
	// handler's in-process keyedMutex still elides the redundant Lookup+Start
	// within a single replica, but it is no longer load-bearing for correctness.
	// The pod name is not a routing handle — the gateway decodes the pod IP from
	// the returned id (ipID) and dials it directly — so naming by key is free.
	podName := containerNameFor(key)
	specBytes, err := json.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}
	pod := r.buildPod(podName, key, cfg, specBytes)

	for {
		createStart := time.Now()
		_, err := r.client.CoreV1().Pods(r.namespace).Create(createCtx, pod, metav1.CreateOptions{})
		if err == nil {
			// We won the key — bring our Pod up.
			return r.bootPod(ctx, key, podName, createStart)
		}
		if !k8serrors.IsAlreadyExists(err) {
			return gen.Sandbox{}, fmt.Errorf("create pod: %w", err)
		}

		// A Pod for this key already exists. Either another caller is bringing it
		// up — in which case get-or-create is idempotent and we wait on theirs —
		// or it is a terminated Pod lingering from a prior run (RestartPolicyNever
		// leaves Succeeded/Failed Pods around) that must be cleared before a fresh
		// one can take the name.
		existing, getErr := r.client.CoreV1().Pods(r.namespace).Get(createCtx, podName, metav1.GetOptions{})
		if k8serrors.IsNotFound(getErr) {
			continue // deleted between our Create and Get; retry the Create
		}
		if getErr != nil {
			return gen.Sandbox{}, fmt.Errorf("get existing pod %s: %w", podName, getErr)
		}
		if !podTerminated(existing.Status.Phase) {
			return r.describeReadyPod(ctx, key, existing)
		}
		if err := r.clearTerminatedPod(podName, existing); err != nil {
			return gen.Sandbox{}, fmt.Errorf("clear terminated pod %s: %w", podName, err)
		}
	}
}

// tryPackCreate is the getOrCreate fast path: if the in-memory pack cache holds a
// warm host for cfg's image, pack key into it via POST /v1/<key> and return
// (ok=true) without touching the k8s API. ok=false means no host is cached for
// the image, so the caller cold-boots a dedicated pod. The POST is idempotent on
// key and candidates maps a key to the same primary host every time, so a repeated
// getOrCreate is safe. The returned id encodes the host's IP, exactly like a
// per-key pod's id, so the gateway dials it directly.
func (r *K8sRuntime) tryPackCreate(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, bool, error) {
	image := r.imageFor(cfg)
	candidates := r.packs.candidates(image, key)
	if len(candidates) == 0 {
		return gen.Sandbox{}, false, nil
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, false, fmt.Errorf("marshal config: %w", err)
	}
	// Try the deterministically-chosen host first, then the rest. Each attempt
	// fails fast: a stale cache entry (a host that has died) fails on connect and
	// we move straight to the next host rather than blocking the request. A
	// definitive rejection from sandboxd is a real error — another host would
	// reject it identically — so it stops the loop. If every cached host is
	// unreachable the cache was stale, so we report ok=false and let the caller
	// cold-boot a dedicated pod.
	for _, ip := range candidates {
		done, retry, err := postSandboxOnce(ctx, ip, key, body)
		switch {
		case done:
			id, idErr := ipID(ip)
			if idErr != nil {
				return gen.Sandbox{}, false, idErr
			}
			log.Printf("sandbox %q: packed into prewarm host %s (image %s)", key, ip, image)
			return gen.Sandbox{Id: id, Key: key}, true, nil
		case retry:
			log.Printf("sandbox %q: prewarm host %s unavailable (%v); trying next", key, ip, err)
		default:
			return gen.Sandbox{}, false, fmt.Errorf("pack %q into prewarm host %s: %w", key, ip, err)
		}
	}
	return gen.Sandbox{}, false, nil
}

// buildPod constructs the per-sandbox Pod. The spec JSON is delivered inline as
// the HIVE_SPEC env var (sandboxd reads it via spec.LoadEnv), the same way the
// docker runtime passes it — so the Pod is self-contained and needs no
// companion ConfigMap.
func (r *K8sRuntime) buildPod(podName, key string, cfg sandboxgen.SandboxConfig, specBytes []byte) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: r.namespace,
			Labels:    map[string]string{labelSandboxKey: key},
		},
		Spec: r.sandboxPodSpec(cfg, specBytes),
	}
}

// sandboxPodSpec builds the per-sandbox PodSpec. sandboxd is launched from the
// inline HIVE_SPEC env, cold-booting its workload at startup.
func (r *K8sRuntime) sandboxPodSpec(cfg sandboxgen.SandboxConfig, specBytes []byte) corev1.PodSpec {
	privileged := true
	// Provision the local snapshot volume only when snapshots aren't routed to a
	// FUSE drive (snapshot.mount); otherwise it's unnecessary and would collide
	// with a FUSE mount at the same path.
	localSnapshots := !usesSnapshotMount(cfg)
	// Always pass --snapshot-dir so Args is non-empty and overrides the image's
	// default `--help` CMD; the dir is empty (local snapshots disabled) when
	// snapshots route to a FUSE drive instead.
	snapDir := "/snapshots"
	if !localSnapshots {
		snapDir = ""
	}
	args := []string{"--snapshot-dir", snapDir}
	// No route_localnet sysctl is set here: for microvm isolation sandboxd
	// enables it from inside the privileged pod (isolation.enableRouteLocalnet),
	// so no unsafe-sysctl node allowlist is required.
	ps := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		HostAliases:   hostAliasesFor(cfg),
		Containers: []corev1.Container{
			{
				Name:  "sandbox",
				Image: r.imageFor(cfg),
				// Default to IfNotPresent so a node with the image already
				// pulled skips the registry round-trip — k8s otherwise forces
				// Always for a :latest tag, adding seconds to every cold pod.
				// Relies on images being pushed under a stable tag/digest.
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            args,
				Ports: []corev1.ContainerPort{
					{ContainerPort: sandboxdPort},
				},
				Env: append(r.envVars(cfg), corev1.EnvVar{Name: spec.EnvSpec, Value: string(specBytes)}),
				// No readiness probe: nothing gates on the pod's Ready
				// condition (there's no per-sandbox Service — the gateway
				// decodes the pod IP from the id and dials it directly).
				// Start and Lookup confirm sandboxd is serving with their own
				// /v1/ping?block=true call (waitSandboxReady), so a kubelet
				// probe would only duplicate that and spam the sandbox log.
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged,
				},
				// Reserve the guest's cpu/memory at the pod level so the
				// scheduler accounts for it; the limit adds host-side overhead
				// (firecracker + sandboxd + sidecars) so the pod isn't OOMKilled.
				Resources: sandboxResources(cfg),
			},
		},
	}
	if localSnapshots {
		ps.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "snapshots", MountPath: "/snapshots"},
		}
		ps.Volumes = []corev1.Volume{r.snapshotVolume()}
	}
	return ps
}

// snapshotVolume returns the /snapshots volume backing sandboxd's --snapshot-dir.
// It is a pod-local ephemeral emptyDir: on the GKE node pool that is GKE-managed
// Local SSD (NVMe), so snapshots land on fast local flash but do not survive the
// Pod. Durable, cross-pod snapshots would need an external store mounted here
// instead. The Firecracker snapshot-resume fast path is unaffected — it snapshots
// to /run/firecracker (the container's NVMe-backed writable layer), not /snapshots.
func (r *K8sRuntime) snapshotVolume() corev1.Volume {
	return corev1.Volume{
		Name:         "snapshots",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// bootPod waits for a freshly-created Pod to schedule and for sandboxd to serve,
// then returns its descriptor. The id encodes the pod IP: we wait for the IP
// (pod scheduled and on the network), then confirm sandboxd is actually serving
// with a block=true ping to the pod directly — the same readiness check the
// docker runtime uses, rather than relying on a kubelet probe's cadence.
func (r *K8sRuntime) bootPod(ctx context.Context, key, podName string, createStart time.Time) (gen.Sandbox, error) {
	log.Printf("sandbox %s: pod created in %s", podName, time.Since(createStart).Round(time.Millisecond))

	ipStart := time.Now()
	scheduledPod, err := r.waitForPodIP(ctx, podName)
	if err != nil {
		return gen.Sandbox{}, err
	}
	podIP := scheduledPod.Status.PodIP
	log.Printf("sandbox %s: pod IP %s assigned (scheduled+pulled) in %s", podName, podIP, time.Since(ipStart).Round(time.Millisecond))
	logPodTimings(scheduledPod)

	readyStart := time.Now()
	if err := waitSandboxReady(ctx, podIP, key); err != nil {
		return gen.Sandbox{}, fmt.Errorf("wait sandbox %s ready: %w", podName, err)
	}
	log.Printf("sandbox %s: sandboxd ready in %s (total start %s)", podName, time.Since(readyStart).Round(time.Millisecond), time.Since(createStart).Round(time.Millisecond))
	routeID, err := ipID(podIP)
	if err != nil {
		return gen.Sandbox{}, err
	}
	return gen.Sandbox{Id: routeID, Key: key}, nil
}

// clearTerminatedPod removes a specific terminated Pod and blocks until it is
// gone (or replaced), so a subsequent Create by the same (key-derived) name can
// take it. The delete is guarded by a UID precondition: it can only ever remove
// the exact terminated Pod observed by the caller, never a live replacement that
// another replica created in the meantime under the same name. Two replicas
// reviving the same key both call this; the precondition makes the loser's
// delete a no-op (the UID no longer matches), and the Create that follows
// arbitrates a single winner.
func (r *K8sRuntime) clearTerminatedPod(name string, pod *corev1.Pod) error {
	ctx, cancel := context.WithTimeout(context.Background(), podIPTimeout)
	defer cancel()

	uid := pod.UID
	zero := int64(0)
	err := r.client.CoreV1().Pods(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	})
	// NotFound: already gone. Conflict: the UID no longer matches — the Pod was
	// already replaced by another caller, so there's nothing for us to clear.
	if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsConflict(err) {
		return err
	}
	for {
		cur, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return nil // the terminated Pod is gone; the outer loop can recreate
		}
		if err != nil {
			return err
		}
		if cur.UID != uid {
			return nil // replaced by a different Pod; the outer Create will resolve it
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pod %s still present after delete", name)
		case <-time.After(readyProbeInterval):
		}
	}
}

// Events streams sandbox lifecycle transitions from two sources merged onto one
// channel: (1) dedicated per-key (cold-boot) Pods, observed via a Pod watch on
// their phase; and (2) sandboxes packed inside prewarm hosts, which aren't their
// own Pods — their transitions come from each host's GET /v1/events SSE, the same
// stream the docker runtime aggregates in eventsPacked. The channel closes once
// both producers stop.
func (r *K8sRuntime) Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
	w, err := r.client.CoreV1().Pods(r.namespace).Watch(ctx, metav1.ListOptions{LabelSelector: labelSandboxKey})
	if err != nil {
		return nil, fmt.Errorf("watch pods: %w", err)
	}
	ch := make(chan gen.SandboxLifecycleEvent, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer w.Stop()
		r.watchPerKeyPods(ctx, w, ch)
	}()
	go func() {
		defer wg.Done()
		r.eventsPacked(ctx, ch)
	}()
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch, nil
}

// watchPerKeyPods maps the phase transitions of dedicated per-key Pods onto out.
// Prewarm host pods are skipped: they carry the label too but never transition
// per packed-sandbox (the host stays Running) — their sandboxes flow through
// eventsPacked instead.
func (r *K8sRuntime) watchPerKeyPods(ctx context.Context, w watch.Interface, out chan<- gen.SandboxLifecycleEvent) {
	// Pods emit many Modified events; track the last phase per pod so each
	// transition is reported once (mirroring docker's discrete actions).
	lastPhase := map[string]corev1.PodPhase{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if _, isPack := prewarmPodImage(pod); isPack {
				continue // host pod, not a sandbox
			}
			key := pod.Labels[labelSandboxKey]
			if key == "" {
				continue
			}
			// The id encodes the pod IP; it's the empty UUID until the pod is
			// scheduled, but every reported transition (Running/Succeeded/
			// Failed/Deleted) happens after the IP is assigned.
			id, _ := ipID(pod.Status.PodIP)
			uid := string(pod.UID)

			var status gen.SandboxLifecycleEventStatus
			switch ev.Type {
			case watch.Deleted:
				delete(lastPhase, uid)
				status = gen.Destroy
			case watch.Added, watch.Modified:
				if lastPhase[uid] == pod.Status.Phase {
					continue // no phase change
				}
				lastPhase[uid] = pod.Status.Phase
				switch pod.Status.Phase {
				case corev1.PodRunning:
					status = gen.Start
				case corev1.PodSucceeded:
					status = gen.Stop
				case corev1.PodFailed:
					status = gen.Die
				default:
					continue // Pending/Unknown: not a reportable transition
				}
			default:
				continue
			}

			select {
			case out <- gen.SandboxLifecycleEvent{Id: id, Key: key, Status: status}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// eventsPacked aggregates the lifecycle streams of every prewarm host. It holds a
// persistent GET /v1/events connection to each host, re-discovering hosts from
// the in-memory pack cache on a ticker (the same cache the getOrCreate fast path
// reads — no pod List here) so new ones are picked up and gone ones dropped, and
// forwards each inner PodEvent onto out keyed by the host's routing id. Mirrors
// the docker runtime's eventsPacked; the per-host streaming reuses streamPodEvents.
func (r *K8sRuntime) eventsPacked(ctx context.Context, out chan<- gen.SandboxLifecycleEvent) {
	conns := map[string]context.CancelCauseFunc{} // pod IP → cancel its stream
	defer func() {
		for _, cancel := range conns {
			cancel(nil) // consumer left: don't mark sandboxes destroyed
		}
	}()
	ticker := time.NewTicker(packCachePollInterval)
	defer ticker.Stop()
	for {
		ips := r.packs.ips()
		seen := make(map[string]bool, len(ips))
		for _, ip := range ips {
			seen[ip] = true
			if _, ok := conns[ip]; ok {
				continue
			}
			id, err := ipID(ip)
			if err != nil {
				continue
			}
			cctx, cancel := context.WithCancelCause(ctx)
			conns[ip] = cancel
			go streamPodEvents(cctx, ctx, ip, id, out)
		}
		for ip, cancel := range conns {
			if !seen[ip] {
				cancel(errPackPodGone) // pod died: streamPodEvents destroys its sandboxes
				delete(conns, ip)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *K8sRuntime) imageFor(cfg sandboxgen.SandboxConfig) string {
	if cfg.Image != nil && *cfg.Image != "" {
		return *cfg.Image
	}
	return defaultSandboxImage
}

// listPackPods returns the running prewarm/pack host pods in the namespace. A
// pack host runs many sandboxes inside one pod — each created via POST /v1/<key>
// — and is the only place List can find the keys it hosts (they aren't their own
// Pods). The getOrCreate fast path and the events stream don't call this on the
// hot path: they read the in-memory pack cache, which a single background poller
// (refreshPackCache) keeps current from here. The controller learns which pods
// are hosts purely from the pod spec, no label required: a pod qualifies when its
// sandbox container carries both --pack and --prewarm (see prewarmPodImage).
func (r *K8sRuntime) listPackPods(ctx context.Context) ([]*corev1.Pod, error) {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var packs []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podTerminated(pod.Status.Phase) {
			continue
		}
		if _, ok := prewarmPodImage(pod); ok {
			packs = append(packs, pod)
		}
	}
	return packs, nil
}

// prewarmPodImage reports the image a pod can serve as a prewarm host, or
// ok=false when the pod is not one. A pod qualifies only when its sandbox
// container carries BOTH the --pack and --prewarm args; the served image is then
// that container's image (the image property). A pod missing either arg, or with
// no image, does not qualify.
func prewarmPodImage(pod *corev1.Pod) (string, bool) {
	c := sandboxContainer(pod)
	if c == nil || !hasArg(c.Args, packArg) || !hasArg(c.Args, prewarmArg) {
		return "", false
	}
	return c.Image, c.Image != ""
}

// sandboxContainer returns the pod's sandbox container, or nil if it has none.
func sandboxContainer(pod *corev1.Pod) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == sandboxContainerName {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// hasArg reports whether args contains the exact token arg.
func hasArg(args []string, arg string) bool {
	for _, a := range args {
		if a == arg {
			return true
		}
	}
	return false
}

func (r *K8sRuntime) envVars(cfg sandboxgen.SandboxConfig) []corev1.EnvVar {
	if cfg.Env == nil {
		return nil
	}
	vars := make([]corev1.EnvVar, 0, len(*cfg.Env))
	for k, v := range *cfg.Env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	return vars
}

// hostAliasesFor translates cfg.ExtraHosts ("hostname:ip") into Pod hostAliases,
// the Kubernetes equivalent of docker's --add-host. Docker's "host-gateway"
// sentinel has no portable Kubernetes equivalent, so such entries are skipped
// rather than mis-resolved.
func hostAliasesFor(cfg sandboxgen.SandboxConfig) []corev1.HostAlias {
	if cfg.ExtraHosts == nil {
		return nil
	}
	var aliases []corev1.HostAlias
	for _, h := range *cfg.ExtraHosts {
		host, ip, ok := strings.Cut(h, ":")
		if !ok || host == "" || ip == "" || ip == "host-gateway" {
			continue
		}
		aliases = append(aliases, corev1.HostAlias{IP: ip, Hostnames: []string{host}})
	}
	return aliases
}
