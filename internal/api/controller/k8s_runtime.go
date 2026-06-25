package controller

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	sandboxdPort = 8099

	// sandboxContainerName is the name of the sandbox container in every prewarm
	// (Deployment) pod. Prewarm discovery resolves the container by this name to
	// read its args and image.
	sandboxContainerName = "sandbox"

	// packArg and prewarmArg are the sandboxd flags that mark a pod as a parked,
	// multi-tenant prewarm host: --pack runs many sandboxes in the one pod, and
	// --prewarm boots sandboxd and waits for the first POST /v1/{key} before
	// launching a workload. A pod can serve prewarmed sandboxes for an image only
	// when its container carries BOTH flags, so prewarm discovery requires both.
	// In the Kubernetes runtime every sandbox pod is a prewarm host managed by a
	// per-image Deployment — the controller never creates pods itself (design §7);
	// creates are routed by the gateway straight to the image's Service.
	packArg    = "--pack"
	prewarmArg = "--prewarm"
)

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

// K8sRuntime implements SandboxRuntime using the Kubernetes API. In Kubernetes
// the controller is a passive observer: it never creates pods (design §7).
// Per-image Deployments run prewarm/pack hosts and the gateway routes creates
// straight to the image's Service (POST /v1/{key}); the controller only lists
// and streams the sandboxes those hosts report. Start therefore returns an
// error and Lookup reports nothing.
type K8sRuntime struct {
	client    kubernetes.Interface
	namespace string
	// packs is the in-memory snapshot of prewarm hosts (image → host IPs) the
	// List path and the events stream read instead of listing pods on every
	// request. A single background poller (startPackCachePoller) refreshes it;
	// see pack_cache.go.
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
	// Burst 10) so the pack-cache poller and List calls aren't throttled. Sized
	// via env so it can track expected concurrency without a rebuild.
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
	// The pack cache backs List and the events stream, so start its poller before
	// serving any request.
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
// still considered alive: it has been created and is coming up, so discovery
// must not treat it as absent.
func podTerminated(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

// Lookup always reports "not running" under Kubernetes: the controller is not
// in the create path (design §7), so it holds no per-key record to resolve.
// Get-or-create idempotency is handled by the image pod's POST /v1/{key}, which
// the gateway routes to directly.
func (r *K8sRuntime) Lookup(ctx context.Context, key string) (bool, gen.Sandbox, error) {
	return false, gen.Sandbox{}, nil
}

// Start is unsupported under Kubernetes: the controller never creates pods
// (design §7). Creates are routed by the gateway on the x-hiver-image header
// straight to the image's Service, which lands on a prewarm pod's POST
// /v1/{key}. This method exists only to satisfy SandboxRuntime and should never
// be reached, since the gateway does not route creates here.
func (r *K8sRuntime) Start(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	return gen.Sandbox{}, fmt.Errorf("k8s runtime does not create sandboxes: creates route directly to the image Service via the gateway (x-hiver-image header)")
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

// List returns every sandbox currently hosted by the prewarm pods. Sandboxes
// live inside the per-image Deployment pods (they aren't their own Pods), so the
// only place to find the keys is the hosts themselves: ask each (GET /v1) for
// the keys it currently hosts and their status, mirroring the docker runtime's
// listPacked. Every key maps to its host's routing id (the pod IP encoded as a
// UUID), exactly the id the create returned.
func (r *K8sRuntime) List(ctx context.Context) ([]gen.Sandbox, error) {
	// Non-nil so an empty result serializes as [] (a JSON null breaks clients
	// that .map the response).
	sandboxes := []gen.Sandbox{}

	packs, err := r.listPackPods(ctx)
	if err != nil {
		return nil, err
	}
	for _, pod := range packs {
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
	return sandboxes, nil
}

// Events streams sandbox lifecycle transitions from the prewarm pods. Each pod
// exposes a GET /v1/events SSE carrying its inner-sandbox transitions; the
// controller follows every host (re-discovered from the in-memory pack cache),
// forwarding each event keyed by the host's routing id. The channel closes once
// the aggregator stops.
func (r *K8sRuntime) Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error) {
	ch := make(chan gen.SandboxLifecycleEvent, 64)
	go func() {
		defer close(ch)
		r.eventsPacked(ctx, ch)
	}()
	return ch, nil
}

// eventsPacked aggregates the lifecycle streams of every prewarm host. It holds a
// persistent GET /v1/events connection to each host, re-discovering hosts from
// the in-memory pack cache on a ticker (the same cache List reads — no pod List
// here) so new ones are picked up and gone ones dropped, and forwards each inner
// PodEvent onto out keyed by the host's routing id. Mirrors the docker runtime's
// eventsPacked; the per-host streaming reuses streamPodEvents.
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

// listPackPods returns the running prewarm/pack host pods in the namespace. In
// Kubernetes these are the per-image Deployment pods: each runs many sandboxes
// inside one pod (created via POST /v1/{key}) and is the only place List can find
// the keys it hosts. The events stream doesn't call this on the hot path: it
// reads the in-memory pack cache, which a single background poller
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
// that container's image. A pod missing either arg, or with no image, does not
// qualify.
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
