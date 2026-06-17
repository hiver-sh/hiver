package warmpool

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// indexResync is the informer's full-resync period. Claims are driven by the
// incremental Add/Update/Delete stream; the resync only re-asserts state as a
// backstop (e.g. re-queuing a pod a transient claim failure dropped), so it can
// be long.
const indexResync = 10 * time.Minute

// readyIndex keeps, per claim hash (SpecHash), a FIFO of warm pods that are
// Running with a PodIP — i.e. claimable right now. It is fed by a shared
// informer watching warm pods in SandboxNS, so Claim selects a candidate from
// local memory instead of a live List against the API server (#1), and pops it
// in O(1) instead of scanning (#2). The pop is atomic, so two concurrent claims
// in this replica never hand out the same pod (#3, intra-replica); the optimistic
// Update in Manager.adopt remains the cross-replica serialization point.
type readyIndex struct {
	factory  informers.SharedInformerFactory
	informer cache.SharedIndexInformer

	mu     sync.Mutex
	queues map[string]*readyQueue // SpecHash -> ready pods
	synced bool
}

// newReadyIndex builds the index and its informer, filtered to warm pods in
// namespace so claimed/other pods never enter the watch (a claim's warm→claimed
// label flip drops the pod from the selector, which the informer delivers as a
// delete — removing it from its queue).
func newReadyIndex(kube kubernetes.Interface, namespace string) *readyIndex {
	factory := informers.NewSharedInformerFactoryWithOptions(
		kube, indexResync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = LabelState + "=" + StateWarm
		}),
	)
	idx := &readyIndex{factory: factory, queues: map[string]*readyQueue{}}
	inf := factory.Core().V1().Pods().Informer()
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { idx.onChange(obj) },
		UpdateFunc: func(_, obj interface{}) { idx.onChange(obj) },
		DeleteFunc: idx.onDelete,
	})
	idx.informer = inf
	return idx
}

// start runs the informer and blocks until its cache has synced (or ctx is
// cancelled). Until synced, hasSynced reports false and Claim falls back to a
// live List, so a freshly (re)started controller can still adopt existing warm
// pods during the sync window. Call once per replica.
func (idx *readyIndex) start(ctx context.Context) {
	idx.factory.Start(ctx.Done())
	if cache.WaitForCacheSync(ctx.Done(), idx.informer.HasSynced) {
		idx.mu.Lock()
		idx.synced = true
		idx.mu.Unlock()
		return
	}
	log.Printf("warmpool: ready index cache sync aborted")
}

func (idx *readyIndex) hasSynced() bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.synced
}

// pop removes and returns the next claimable pod for hash, or nil if none are
// queued. The removal is the intra-replica single-winner guarantee.
func (idx *readyIndex) pop(hash string) *corev1.Pod {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	q := idx.queues[hash]
	if q == nil {
		return nil
	}
	return q.pop()
}

// onChange (Add/Update) queues a pod that is warm and Running with an IP, and
// removes one that is not (e.g. still Pending). Idempotent: repeated events and
// resyncs don't double-enqueue.
func (idx *readyIndex) onChange(obj interface{}) {
	pod := toPod(obj)
	if pod == nil {
		return
	}
	ready := pod.DeletionTimestamp == nil &&
		pod.Labels[LabelState] == StateWarm &&
		pod.Status.Phase == corev1.PodRunning &&
		pod.Status.PodIP != ""
	idx.mu.Lock()
	defer idx.mu.Unlock()
	hash := pod.Labels[LabelSpec]
	q := idx.queues[hash]
	if ready {
		if q == nil {
			q = newReadyQueue()
			idx.queues[hash] = q
		}
		q.add(pod)
	} else if q != nil {
		q.remove(pod.Name)
	}
}

func (idx *readyIndex) onDelete(obj interface{}) {
	pod := toPod(obj)
	if pod == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if q := idx.queues[pod.Labels[LabelSpec]]; q != nil {
		q.remove(pod.Name)
	}
}

// readyQueue is a FIFO of claimable pods for one SpecHash. member dedupes
// enqueues; pods holds the latest object per name; order is the FIFO of names,
// from which removed entries are skipped lazily on pop.
type readyQueue struct {
	order  []string
	member map[string]struct{}
	pods   map[string]*corev1.Pod
}

func newReadyQueue() *readyQueue {
	return &readyQueue{member: map[string]struct{}{}, pods: map[string]*corev1.Pod{}}
}

func (q *readyQueue) add(p *corev1.Pod) {
	q.pods[p.Name] = p // refresh the stored object even if already queued
	if _, ok := q.member[p.Name]; ok {
		return
	}
	q.member[p.Name] = struct{}{}
	q.order = append(q.order, p.Name)
}

func (q *readyQueue) remove(name string) {
	delete(q.member, name)
	delete(q.pods, name)
}

func (q *readyQueue) pop() *corev1.Pod {
	for len(q.order) > 0 {
		name := q.order[0]
		q.order = q.order[1:]
		if _, ok := q.member[name]; !ok {
			continue // removed since it was enqueued
		}
		p := q.pods[name]
		q.remove(name)
		return p
	}
	q.order = nil // fully drained: release the backing array
	return nil
}

// toPod unwraps an informer object to a *Pod, handling the tombstone the delete
// handler may receive after a missed watch event.
func toPod(obj interface{}) *corev1.Pod {
	switch o := obj.(type) {
	case *corev1.Pod:
		return o
	case cache.DeletedFinalStateUnknown:
		if p, ok := o.Obj.(*corev1.Pod); ok {
			return p
		}
	}
	return nil
}
