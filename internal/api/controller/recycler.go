package controller

import (
	"context"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// envCompletedPodTTL configures how long a sandbox pod is held in its
	// completed (Succeeded/Failed) state before the recycler deletes it and
	// reclaims its resources. Parsed as a Go duration ("30s", "5m", "1h"). Unset
	// falls back to defaultCompletedPodTTL; "0" (or any non-positive value)
	// disables recycling, leaving terminated pods to be cleared lazily by a later
	// Start that reuses the same key.
	envCompletedPodTTL = "HIVE_COMPLETED_POD_TTL"

	// defaultCompletedPodTTL is the hold time used when envCompletedPodTTL is
	// unset: long enough that a just-finished sandbox's pod (and its logs/status)
	// can still be inspected briefly, short enough that completed pods don't pile
	// up against the namespace's pod count.
	defaultCompletedPodTTL = 5 * time.Minute

	// recycleSweepCap bounds how often the recycler sweeps regardless of TTL, so
	// a long TTL still wakes periodically (and a freshly-expired pod is reaped
	// within roughly this window past its TTL rather than a full TTL later).
	recycleSweepCap = 1 * time.Minute
)

// completedPodTTL resolves the configured hold time. A return of <=0 means
// recycling is disabled.
func completedPodTTL() time.Duration {
	v := os.Getenv(envCompletedPodTTL)
	if v == "" {
		return defaultCompletedPodTTL
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("recycler: invalid %s=%q: %v; using default %s", envCompletedPodTTL, v, err, defaultCompletedPodTTL)
		return defaultCompletedPodTTL
	}
	return d
}

// startRecycler launches a background sweep that deletes sandbox pods which have
// sat in a completed (Succeeded/Failed) state longer than the configured TTL,
// reclaiming their resources. Sandbox pods use RestartPolicyNever, so a finished
// workload leaves a terminated pod behind; without this sweep those linger until
// a later Start reuses the same key (clearTerminatedPod). It runs on every
// replica — deletes are UID-guarded and NotFound-tolerant, so concurrent sweeps
// across replicas are harmless. A non-positive TTL disables it.
func (r *K8sRuntime) startRecycler(ctx context.Context) {
	ttl := completedPodTTL()
	if ttl <= 0 {
		log.Printf("recycler: disabled (%s<=0)", envCompletedPodTTL)
		return
	}
	interval := ttl
	if interval > recycleSweepCap {
		interval = recycleSweepCap
	}
	log.Printf("recycler: holding completed pods for %s, sweeping every %s in %s", ttl, interval, r.namespace)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		r.recycleCompleted(ctx, ttl) // act immediately rather than after one interval
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.recycleCompleted(ctx, ttl)
			}
		}
	}()
}

// recycleCompleted reaps terminated sandbox pods the cluster won't GC on its own
// (all run RestartPolicyNever): claimed and cold-boot sandboxes, which carry the
// sandbox-key label.
func (r *K8sRuntime) recycleCompleted(ctx context.Context, ttl time.Duration) {
	r.sweepCompleted(ctx, ttl, labelSandboxKey)
}

// sweepCompleted lists pods matching selector and deletes those terminated longer
// than ttl ago. Per-pod failures are logged and skipped so one bad delete can't
// stall the sweep.
func (r *K8sRuntime) sweepCompleted(ctx context.Context, ttl time.Duration, selector string) {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		log.Printf("recycler: list pods (%s): %v", selector, err)
		return
	}
	now := time.Now()
	for i := range pods.Items {
		p := &pods.Items[i]
		if !podTerminated(p.Status.Phase) {
			continue
		}
		done := completedAt(p)
		if now.Sub(done) < ttl {
			continue
		}
		// UID precondition: only ever delete the exact terminated pod observed
		// here, never a live replacement another caller created under the same
		// name between our List and Delete.
		uid := p.UID
		err := r.client.CoreV1().Pods(r.namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsConflict(err) {
			log.Printf("recycler: delete completed pod %s: %v", p.Name, err)
			continue
		}
		log.Printf("recycler: reclaimed completed pod %s (key %q, completed %s ago)",
			p.Name, p.Labels[labelSandboxKey], now.Sub(done).Round(time.Second))
	}
}

// completedAt estimates when a terminated pod finished, preferring the latest
// container termination time (the most precise signal) and falling back to the
// pod's start or creation timestamp when container statuses are absent. The
// fallback is conservative — it can only make a pod look older, never younger,
// so a pod is never recycled before its real completion plus TTL.
func completedAt(p *corev1.Pod) time.Time {
	var done time.Time
	for _, cs := range p.Status.ContainerStatuses {
		if term := cs.State.Terminated; term != nil && term.FinishedAt.Time.After(done) {
			done = term.FinishedAt.Time
		}
	}
	if !done.IsZero() {
		return done
	}
	if p.Status.StartTime != nil {
		return p.Status.StartTime.Time
	}
	return p.CreationTimestamp.Time
}
