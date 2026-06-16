package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/spec"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const sandboxdPort = 8099

// podIPTimeout bounds how long Start/Lookup wait for the scheduler to assign the
// pod an IP. This covers scheduling and image pull on a cold node; sandboxd boot
// is waited for separately by waitSandboxReady (the /v1/ping?block=true call).
const podIPTimeout = 120 * time.Second

// K8sRuntime implements SandboxRuntime using the Kubernetes API.
type K8sRuntime struct {
	client    kubernetes.Interface
	namespace string
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
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	ns := os.Getenv("HIVE_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &K8sRuntime{client: client, namespace: ns}, nil
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
	privileged := true
	// No route_localnet sysctl is set here: for microvm isolation sandboxd
	// enables it from inside the privileged pod (isolation.enableRouteLocalnet),
	// so no unsafe-sysctl node allowlist is required.
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: r.namespace, Labels: map[string]string{labelSandboxKey: key}},
		Spec: corev1.PodSpec{
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
					Args:            []string{"--snapshot-dir", "/snapshots"},
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
					VolumeMounts: []corev1.VolumeMount{
						{Name: "snapshots", MountPath: "/snapshots"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					// sandboxd requires --snapshot-dir for snapshot support. This is
					// pod-local and ephemeral; durable, cross-pod snapshots need an
					// RWX PersistentVolume (e.g. Filestore) mounted here instead.
					Name: "snapshots",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
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
