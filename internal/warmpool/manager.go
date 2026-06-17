package warmpool

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	// leaseName is the Lease all controller replicas contend for; only the holder
	// runs the reconcile loop (the replenisher is singleton work). Claims are not
	// gated on leadership — they're safe concurrently via optimistic updates.
	leaseName = "warmpool-controller"
	// reconcileInterval is how often the leader re-derives every pool's warm
	// buffer. A pod watch would react faster, but a periodic sweep is simple and
	// self-correcting: a claim shrinks the buffer and the next tick replenishes.
	reconcileInterval = 5 * time.Second
)

// Config wires a Manager to the cluster. The build factory and keyLabel come
// from the controller's K8s runtime so pod construction and the sandbox-key
// labelling convention stay owned in one place.
type Config struct {
	Kube      kubernetes.Interface
	Dynamic   dynamic.Interface
	ControlNS string // namespace holding WarmPool CRs and the Lease (the controller's own)
	SandboxNS string // namespace the warm/claimed pods live in
	KeyLabel  string // the runtime's sandbox-key label, set on a pod at claim time
	Identity  string // unique leader-election identity for this replica
	// BuildWarm constructs a warm pod for a pool and one of its image buffers. A
	// pool can keep several different images warm, so the image entry is passed
	// explicitly rather than read off the pool.
	BuildWarm func(wp WarmPool, img WarmPoolImage) *corev1.Pod
}

// Manager runs the warm-pool reconciler (leader-elected) and serves claims (on
// every replica).
type Manager struct {
	cfg   Config
	index *readyIndex // informer-backed claim fast-path; ready on every replica
}

func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg, index: newReadyIndex(cfg.Kube, cfg.SandboxNS)}
}

// Run blocks, contending for leadership and running the reconcile loop while
// this replica is the leader. It returns when ctx is cancelled. Call it in its
// own goroutine.
func (m *Manager) Run(ctx context.Context) {
	// The claim fast-path runs on every replica, independent of leadership: start
	// (and sync) the informer here, before blocking on leader election below.
	m.index.start(ctx)

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: m.cfg.ControlNS},
		Client:     m.cfg.Kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: m.cfg.Identity},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Name:            leaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: m.reconcileLoop,
			OnStoppedLeading: func() { log.Printf("warmpool: lost leadership (%s)", m.cfg.Identity) },
		},
	})
}

// reconcileLoop runs the periodic sweep until leadership is lost (ctx cancelled
// by the leader-election machinery).
func (m *Manager) reconcileLoop(ctx context.Context) {
	log.Printf("warmpool: leading (%s); reconciling every %s", m.cfg.Identity, reconcileInterval)
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	m.reconcileAll(ctx) // act immediately on acquiring leadership
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcileAll(ctx)
		}
	}
}

// reconcileAll converges every WarmPool in the control namespace, then reaps
// warm pods whose pool no longer exists. Per-pool failures are logged and skipped
// so one bad pool can't stall the rest.
func (m *Manager) reconcileAll(ctx context.Context) {
	list, err := m.cfg.Dynamic.Resource(GVR).Namespace(m.cfg.ControlNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("warmpool: list pools: %v", err)
		return
	}
	// The live set is keyed off the raw object names, independent of whether a
	// pool decodes — so a transient decode error never makes reapOrphans mistake
	// a real pool's pods for orphans.
	live := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		live[list.Items[i].GetName()] = true
	}
	for i := range list.Items {
		var wp WarmPool
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &wp); err != nil {
			log.Printf("warmpool: decode pool %s: %v", list.Items[i].GetName(), err)
			continue
		}
		if err := m.reconcilePool(ctx, wp); err != nil {
			log.Printf("warmpool: reconcile %s: %v", wp.Name, err)
		}
	}
	m.reapOrphans(ctx, live)
}

// reapOrphans deletes warm pods whose owning pool has been deleted. Cross-
// namespace ownerReferences can't drive this (the CRs live in ControlNS, the
// pods in SandboxNS, and k8s GC forbids cross-namespace owners), so the sweep is
// explicit. Only warm pods are reaped; a claimed pod is a live sandbox and
// drains naturally on its own Shutdown.
func (m *Manager) reapOrphans(ctx context.Context, live map[string]bool) {
	pods, err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: LabelState + "=" + StateWarm,
	})
	if err != nil {
		log.Printf("warmpool: list for orphan sweep: %v", err)
		return
	}
	var orphans []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if podTerminated(p.Status.Phase) {
			continue
		}
		if pool := p.Labels[LabelPool]; pool != "" && !live[pool] {
			orphans = append(orphans, p)
		}
	}
	m.deleteWarm(ctx, orphans)
}

// reconcilePool brings one pool's warm buffer to its target and writes status.
func (m *Manager) reconcilePool(ctx context.Context, wp WarmPool) error {
	pods, err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: LabelPool + "=" + wp.Name,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	// Each image entry is an independent buffer keyed by its claim hash (SpecHash
	// of the image), with its own readyReplicas/maxReplicas. Index the
	// pool's current images by that hash; a warm pod whose hash matches none of
	// them can never be claimed for this spec, so it's stale and gets drained.
	// Claimed pods count toward their own image's maxReplicas (a stale one whose
	// image left the pool counts toward none, and drains on its own Shutdown).
	type imgState struct {
		spec    WarmPoolImage
		warm    []*corev1.Pod
		claimed int
		ready   int
	}
	byHash := make(map[string]*imgState, len(wp.Spec.Images))
	order := make([]string, 0, len(wp.Spec.Images))
	for _, img := range wp.Spec.Images {
		h := SpecHash(img.Image)
		if _, dup := byHash[h]; dup {
			continue
		}
		byHash[h] = &imgState{spec: img}
		order = append(order, h)
	}

	var stale []*corev1.Pod
	var totalReady, totalClaimed int
	for i := range pods.Items {
		p := &pods.Items[i]
		if podTerminated(p.Status.Phase) {
			continue
		}
		st := byHash[p.Labels[LabelSpec]]
		switch p.Labels[LabelState] {
		case StateClaimed:
			totalClaimed++
			if st != nil {
				st.claimed++
			}
		case StateWarm:
			if st == nil {
				stale = append(stale, p)
				continue
			}
			st.warm = append(st.warm, p)
			if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
				st.ready++
				totalReady++
			}
		}
	}

	// Reap stale warm pods unconditionally (they're uncountable toward any target),
	// then converge each image's buffer to its own target.
	m.deleteWarm(ctx, stale)
	for _, h := range order {
		st := byHash[h]
		target := st.spec.WarmTarget(st.claimed)
		switch {
		case len(st.warm) < target:
			m.createWarm(ctx, wp, st.spec, target-len(st.warm))
		case len(st.warm) > target:
			m.deleteWarm(ctx, pickEvictable(st.warm, len(st.warm)-target))
		}
	}

	return m.writeStatus(ctx, wp, totalReady, totalClaimed)
}

// createWarm pre-boots n warm pods of img for the pool. Creates are best-effort:
// a failure is logged and the next sweep retries, so a transient API error
// doesn't abort the pool.
func (m *Manager) createWarm(ctx context.Context, wp WarmPool, img WarmPoolImage, n int) {
	for i := 0; i < n; i++ {
		pod := m.cfg.BuildWarm(wp, img)
		if _, err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			log.Printf("warmpool: create warm pod for %s (%s): %v", wp.Name, img.Image, err)
			return
		}
	}
}

// deleteWarm removes surplus warm pods (e.g. after maxReplicas shrank or claims
// grew). Best-effort and idempotent: an already-gone pod is fine.
func (m *Manager) deleteWarm(ctx context.Context, victims []*corev1.Pod) {
	for _, p := range victims {
		if err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			log.Printf("warmpool: delete surplus warm pod %s: %v", p.Name, err)
		}
	}
}

// writeStatus records observed counts on the WarmPool's status subresource.
func (m *Manager) writeStatus(ctx context.Context, wp WarmPool, ready, claimed int) error {
	cur, err := m.cfg.Dynamic.Resource(GVR).Namespace(m.cfg.ControlNS).Get(ctx, wp.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get for status: %w", err)
	}
	status := map[string]interface{}{
		"readyReplicas":      int64(ready),
		"claimedReplicas":    int64(claimed),
		"observedGeneration": cur.GetGeneration(),
	}
	if err := unstructured.SetNestedMap(cur.Object, status, "status"); err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	if _, err := m.cfg.Dynamic.Resource(GVR).Namespace(m.cfg.ControlNS).UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// Claim atomically adopts a ready warm pod matching image for key, returning it
// and true, or (nil, false, nil) when the pool is empty or every candidate was
// raced away by another replica. The adopted pod gains the sandbox-key label and
// flips to claimed, so the runtime's key-based lookups now see it as the sandbox
// for key. The caller still delivers the real config to the pod's sandboxd (the
// prewarm pod is parked awaiting its first PUT /v1/config).
func (m *Manager) Claim(ctx context.Context, image, key string) (*corev1.Pod, bool, error) {
	hash := SpecHash(image)
	// Fast path: pop claimable pods from the informer-fed index (local memory, no
	// API round-trip to select a candidate). The pop is atomic, so concurrent
	// claims in this replica never contend for the same pod; adopt's optimistic
	// Update guards the cross-replica race. A candidate the Update loses (Conflict/
	// NotFound) is dropped and we pop the next.
	if m.index.hasSynced() {
		for {
			p := m.index.pop(hash)
			if p == nil {
				return nil, false, nil
			}
			pod, ok, err := m.adopt(ctx, p, key)
			if err != nil {
				return nil, false, err
			}
			if ok {
				return pod, true, nil
			}
		}
	}
	// Fallback while the index is still syncing (e.g. just after startup): a live
	// List so existing warm pods are still claimable.
	return m.claimByList(ctx, hash, key)
}

// adopt flips one warm pod to claimed for key via an optimistic Update. It
// returns ok=true on success; ok=false (err=nil) when the pod was raced away by
// another replica or reaped between selection and Update (Conflict/NotFound), so
// the caller should try the next candidate; and a non-nil err on any other
// failure.
func (m *Manager) adopt(ctx context.Context, p *corev1.Pod, key string) (*corev1.Pod, bool, error) {
	claimed := p.DeepCopy()
	if claimed.Labels == nil {
		claimed.Labels = map[string]string{}
	}
	claimed.Labels[m.cfg.KeyLabel] = key
	claimed.Labels[LabelState] = StateClaimed
	// Update carries the pod's resourceVersion, so two replicas racing the same
	// warm pod can't both win: the loser gets a Conflict. A NotFound means the
	// reconciler reaped the pod (surplus/orphan) since we selected it.
	updated, err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).Update(ctx, claimed, metav1.UpdateOptions{})
	if err == nil {
		return updated, true, nil
	}
	if k8serrors.IsConflict(err) || k8serrors.IsNotFound(err) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("claim warm pod %s: %w", p.Name, err)
}

// claimByList is the pre-index claim path: a live List of warm pods matching
// hash, then adopt the first that is scheduled and on the network. Used only
// until the index has synced.
func (m *Manager) claimByList(ctx context.Context, hash, key string) (*corev1.Pod, bool, error) {
	sel := fmt.Sprintf("%s=%s,%s=%s", LabelSpec, hash, LabelState, StateWarm)
	pods, err := m.cfg.Kube.CoreV1().Pods(m.cfg.SandboxNS).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, false, fmt.Errorf("list warm pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		// Only hand out a pod that is scheduled and on the network: sandboxd is
		// then serving and can accept the config PUT.
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		pod, ok, err := m.adopt(ctx, p, key)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return pod, true, nil
		}
	}
	return nil, false, nil
}

// pickEvictable chooses up to n warm pods to delete, preferring those not yet
// running (cheapest to discard) over ready ones that a claim might still take.
func pickEvictable(warm []*corev1.Pod, n int) []*corev1.Pod {
	if n >= len(warm) {
		return warm
	}
	notReady := make([]*corev1.Pod, 0, len(warm))
	rest := make([]*corev1.Pod, 0, len(warm))
	for _, p := range warm {
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			notReady = append(notReady, p)
		} else {
			rest = append(rest, p)
		}
	}
	ordered := append(notReady, rest...)
	return ordered[:n]
}

func podTerminated(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}
