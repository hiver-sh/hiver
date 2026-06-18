package controller

import (
	"bytes"
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
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/spec"
	"github.com/hiver-sh/hiver/internal/warmpool"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	sandboxdPort = 8099

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

	// defaultRequestCPU is the pod CPU *request* (cores) used when a config
	// leaves request_cpu unset. It mirrors the api/config.yaml default and is
	// deliberately below defaultVcpuCount so an idle sandbox reserves less than
	// the limit it can burst to, letting many warm/idle pods pack onto a node.
	defaultRequestCPU = 0.5

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
// overpack a node). CPU limit = cpu (the ceiling the guest can burst to); CPU
// request = request_cpu (what the scheduler reserves), clamped to at most the
// limit. Decoupling the two lets an idle sandbox reserve less than it can use.
// Memory request = guest size; the memory limit adds host-side overhead for
// firecracker + sandboxd + sidecars (see podMemoryOverheadMiB).
func sandboxResources(cfg sandboxgen.SandboxConfig) corev1.ResourceRequirements {
	cpuLimit := defaultVcpuCount
	if cfg.Cpu != nil && *cfg.Cpu > 0 {
		cpuLimit = *cfg.Cpu
	}
	cpuReq := defaultRequestCPU
	if cfg.RequestCpu != nil && *cfg.RequestCpu > 0 {
		cpuReq = *cfg.RequestCpu
	}
	// A request above the limit is rejected by the API server; clamp it.
	cpuReq = math.Min(cpuReq, cpuLimit)
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

// sandboxHTTPClient talks to sandboxd over the pod network. A claim hits the
// same pod IP twice back to back — applyConfig's PUT /v1/config then
// waitSandboxReady's GET /v1/ping — so a keep-alive transport lets the GET reuse
// the PUT's TCP connection, saving a connect+handshake on the latency path. (The
// callers must drain response bodies for the connection to return to the pool.)
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
	// warm is the warm-pool manager: it pre-boots prewarm pods (leader-elected)
	// and lets Start adopt one instead of cold-booting. nil if warm pooling
	// failed to initialise, in which case Start always cold-boots.
	warm *warmpool.Manager
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
	r.startWarmPool(config)
	r.startRecycler(context.Background())
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

// startWarmPool wires up and launches the warm-pool manager. Failure to build
// the dynamic client is logged and tolerated: the controller still serves
// requests, just always cold-booting (r.warm stays nil).
func (r *K8sRuntime) startWarmPool(config *rest.Config) {
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Printf("warmpool: dynamic client: %v; warm pooling disabled", err)
		return
	}
	r.warm = warmpool.NewManager(warmpool.Config{
		Kube:      r.client,
		Dynamic:   dyn,
		ControlNS: controlNamespace(),
		SandboxNS: r.namespace,
		KeyLabel:  labelSandboxKey,
		Identity:  leaderIdentity(),
		BuildWarm: r.buildWarmPod,
	})
	go r.warm.Run(context.Background())
}

// controlNamespace is where WarmPool CRs and the leader-election Lease live: the
// controller's own namespace. It's read from the ServiceAccount token (the
// in-cluster source of truth), then HIVE_CONTROL_NAMESPACE, falling back to the
// deployment's "hiver".
func controlNamespace() string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	if ns := os.Getenv("HIVE_CONTROL_NAMESPACE"); ns != "" {
		return ns
	}
	return "hiver"
}

// leaderIdentity is a per-replica leader-election identity: hostname (the pod
// name under k8s) plus a random suffix so two replicas can never collide.
func leaderIdentity() string {
	host, _ := os.Hostname()
	return host + "_" + uuid.NewString()
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
	if err := waitSandboxReady(ctx, ip); err != nil {
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

// podForKey returns the Pod carrying key, preferring a live one, or nil if the
// key has no Pod at all. Pods are named by id, so the caller-chosen key is
// resolved through the hiver.sandbox.key label rather than the object name.
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
		if !podTerminated(p.Status.Phase) {
			return p, nil
		}
		chosen = p
	}
	return chosen, nil
}

func (r *K8sRuntime) List(ctx context.Context) ([]gen.Sandbox, error) {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSandboxKey,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	sandboxes := make([]gen.Sandbox, 0, len(pods.Items))
	for _, pod := range pods.Items {
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
	// Fast path: adopt a pre-booted warm pod whose image matches this
	// request. A miss (no pool, empty buffer, or delivery failure) falls through
	// to the cold-boot path below, so warm pooling is a pure optimisation.
	if sb, ok := r.claimWarm(ctx, key, cfg); ok {
		return sb, nil
	}

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
		Spec: r.sandboxPodSpec(cfg, specBytes, false),
	}
}

// sandboxPodSpec builds the PodSpec shared by cold-boot and warm (prewarm) pods.
// When prewarm is true sandboxd is launched with --prewarm: it brings up its API
// and parks without a workload until the first PUT /v1/config supplies the real
// spec (see cmd/sandboxd prewarm path).
func (r *K8sRuntime) sandboxPodSpec(cfg sandboxgen.SandboxConfig, specBytes []byte, prewarm bool) corev1.PodSpec {
	privileged := true
	// Provision the local snapshot volume only when snapshots aren't routed to a
	// FUSE drive (snapshot.mount); otherwise it's unnecessary and would collide
	// with a FUSE mount at the same path. A prewarm pod has no config yet, so it
	// keeps the default volume and only relinquishes it once a non-prewarm spec
	// is known.
	localSnapshots := !usesSnapshotMount(cfg)
	// Always pass --snapshot-dir so Args is non-empty and overrides the image's
	// default `--help` CMD; the dir is empty (local snapshots disabled) when
	// snapshots route to a FUSE drive instead.
	snapDir := "/snapshots"
	if !localSnapshots {
		snapDir = ""
	}
	args := []string{"--snapshot-dir", snapDir}
	if prewarm {
		args = append(args, "--prewarm")
	}
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
// instead. The Firecracker prewarm fast path is unaffected — it snapshots to
// /run/firecracker (the container's NVMe-backed writable layer), not /snapshots.
func (r *K8sRuntime) snapshotVolume() corev1.Volume {
	return corev1.Volume{
		Name:         "snapshots",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// buildWarmPod constructs an unclaimed warm pod for one of a WarmPool's image
// buffers: a prewarm sandboxd carrying only that image (the field frozen at
// boot), parked until a claim delivers the rest via PUT /v1/config. The image
// also fixes the isolation, which sandboxd derives from it at boot. It is named
// by GenerateName and labelled for the warm-pool machinery — deliberately
// without a sandbox-key label, so the runtime's key-based Lookup/List/Events
// don't surface it as a sandbox until Claim adds the key. Passed as the
// BuildWarm factory to the warmpool.Manager.
func (r *K8sRuntime) buildWarmPod(wp warmpool.WarmPool, img warmpool.WarmPoolImage) *corev1.Pod {
	image := img.Image
	cfg := sandboxgen.SandboxConfig{Image: &image}
	// Size the warm guest from the pool spec: cpu/memory are frozen at the warm
	// pod's prewarm boot (HIVE_SPEC -> sandboxd -> isolation), so a claim adopts a
	// pod already sized here. They also flow into the pod's resource requests via
	// sandboxPodSpec. Zero leaves them unset, defaulting in sandboxd / the runtime.
	if img.Cpu > 0 {
		cpu := img.Cpu
		cfg.Cpu = &cpu
	}
	if img.RequestCpu > 0 {
		reqCPU := img.RequestCpu
		cfg.RequestCpu = &reqCPU
	}
	if img.Memory > 0 {
		mem := img.Memory
		cfg.Memory = &mem
	}
	specBytes, _ := json.Marshal(cfg)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "warm-" + wp.Name + "-",
			Namespace:    r.namespace,
			Labels: map[string]string{
				warmpool.LabelPool:  wp.Name,
				warmpool.LabelSpec:  warmpool.SpecHash(image),
				warmpool.LabelState: warmpool.StateWarm,
			},
		},
		Spec: r.sandboxPodSpec(cfg, specBytes, true),
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
	if err := waitSandboxReady(ctx, podIP); err != nil {
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

// Events streams sandbox lifecycle transitions by watching the labelled Pods.
func (r *K8sRuntime) Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
	w, err := r.client.CoreV1().Pods(r.namespace).Watch(ctx, metav1.ListOptions{LabelSelector: labelSandboxKey})
	if err != nil {
		return nil, fmt.Errorf("watch pods: %w", err)
	}
	ch := make(chan gen.SandboxLifecycleEvent, 16)
	go func() {
		defer close(ch)
		defer w.Stop()
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
				case ch <- gen.SandboxLifecycleEvent{Id: id, Key: key, Status: status}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (r *K8sRuntime) Shutdown(ctx context.Context, key string) error {
	// The key-named Pod is the sandbox. The same-named ConfigMap is only a
	// legacy artifact: older controllers created a per-key anchor ConfigMap that
	// Start no longer produces, so we still look it up and delete it to clean up
	// any left over across a rollout, but it is no longer part of the model.
	anchorName := containerNameFor(key)

	pod, err := r.podForKey(key)
	if err != nil {
		return err
	}
	_, anchorErr := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, anchorName, metav1.GetOptions{})
	if anchorErr != nil && !k8serrors.IsNotFound(anchorErr) {
		return fmt.Errorf("get anchor configmap %s: %w", anchorName, anchorErr)
	}
	// Neither a live/terminated Pod nor a legacy anchor: nothing to remove.
	if pod == nil && k8serrors.IsNotFound(anchorErr) {
		return ErrSandboxNotFound
	}

	if pod != nil {
		if err := r.client.CoreV1().Pods(r.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
	}
	// Best-effort cleanup of a legacy anchor ConfigMap, if one exists.
	if err := r.client.CoreV1().ConfigMaps(r.namespace).Delete(ctx, anchorName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete anchor configmap %s: %w", anchorName, err)
	}
	return nil
}

// claimWarm tries to satisfy the request from the warm pool: adopt a matching
// prewarm pod for key, deliver the real config to its sandboxd, and wait for the
// workload to come up. It returns ok=false on any miss or failure so Start falls
// back to a normal cold boot — never surfacing a warm-pool problem to the caller.
func (r *K8sRuntime) claimWarm(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, bool) {
	if r.warm == nil {
		return gen.Sandbox{}, false
	}
	image := r.imageFor(cfg)
	pod, ok, err := r.warm.Claim(ctx, image, key)
	if err != nil {
		log.Printf("sandbox %q: warm claim failed, cold-booting: %v", key, err)
		return gen.Sandbox{}, false
	}
	if !ok {
		return gen.Sandbox{}, false
	}

	claimStart := time.Now()
	ip := pod.Status.PodIP
	// Deliver the real config: a prewarm sandboxd is parked awaiting its first
	// PUT /v1/config, which latches the workload launch. The image was frozen by
	// the warm pod's boot env (and fixes the isolation), so this only supplies
	// the rest.
	if err := r.applyConfig(ctx, ip, cfg); err != nil {
		log.Printf("sandbox %q: warm pod %s config delivery failed: %v", key, pod.Name, err)
		// The pod is already claimed (labelled with key); tear it down so the
		// cold-boot Create below can take the key cleanly and we don't leak it.
		_ = r.client.CoreV1().Pods(r.namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		return gen.Sandbox{}, false
	}
	if err := waitSandboxReady(ctx, ip); err != nil {
		log.Printf("sandbox %q: warm pod %s not ready after config: %v", key, pod.Name, err)
		_ = r.client.CoreV1().Pods(r.namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		return gen.Sandbox{}, false
	}
	id, err := ipID(ip)
	if err != nil {
		return gen.Sandbox{}, false
	}
	log.Printf("sandbox %q: claimed warm pod %s in %s", key, pod.Name, time.Since(claimStart).Round(time.Millisecond))
	return gen.Sandbox{Id: id, Key: key}, true
}

// applyConfig delivers cfg to the prewarm sandboxd at ip via PUT /v1/config. The
// PUT is retried while the connection is refused: a just-Running warm pod may
// not have sandboxd bound yet (the same reason waitSandboxReady retries its GET).
func (r *K8sRuntime) applyConfig(ctx context.Context, ip string, cfg sandboxgen.SandboxConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxReadyTimeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/config", ip, sandboxdPort)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := sandboxHTTPClient.Do(req)
		if err == nil {
			status := resp.StatusCode
			drainAndClose(resp) // return the connection to the pool for the ready GET
			if status == http.StatusOK {
				return nil
			}
			return fmt.Errorf("config PUT returned %s", resp.Status)
		}
		// sandboxd not listening yet: back off and retry while time remains.
		select {
		case <-ctx.Done():
			return fmt.Errorf("config PUT to %s: %w", ip, ctx.Err())
		case <-time.After(readyProbeInterval):
		}
	}
}

func (r *K8sRuntime) imageFor(cfg sandboxgen.SandboxConfig) string {
	if cfg.Image != nil && *cfg.Image != "" {
		return *cfg.Image
	}
	return defaultSandboxImage
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
