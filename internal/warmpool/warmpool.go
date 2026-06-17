// Package warmpool implements the WarmPool custom resource: a leader-elected
// reconciler that keeps a buffer of pre-booted (prewarm) sandbox pods on standby,
// and a Claim handshake that hands one to a GetOrCreate request instead of
// cold-booting. Pods are decoded from the dynamic client into the typed structs
// below with runtime.DefaultUnstructuredConverter, so no generated clientset or
// scheme registration is needed — keeping the dependency surface to the client-go
// the controller already uses.
//
// The package owns the WarmPool API types, the pod labels that mark warm/claimed
// pods, and the orchestration (Manager); the controller's K8s runtime owns pod
// construction (buildWarmPod) and request-time delivery of the real config. The
// two coordinate purely through the Kubernetes API (pod labels + optimistic
// updates), so claims are safe across controller replicas without a shared
// process.
package warmpool

import (
	"crypto/sha256"
	"encoding/hex"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVR identifies the WarmPool custom resource (warmpools.hiver.sh).
var GVR = schema.GroupVersionResource{Group: "hiver.sh", Version: "v1alpha1", Resource: "warmpools"}

const (
	// LabelPool names the WarmPool a pod was pre-booted for. It stays on the pod
	// after a claim so the reconciler can keep counting it toward the pool's
	// claimed total (and so status reflects in-use pods).
	LabelPool = "hiver.sandbox.pool"
	// LabelSpec is a hash of the pod's image — the claim key. A GetOrCreate only
	// adopts a warm pod whose LabelSpec matches its request, because the image is
	// frozen at boot and can't be changed by the config the claim delivers (and
	// the image determines isolation, which sandboxd derives from it).
	LabelSpec = "hiver.sandbox.spec"
	// LabelState is StateWarm while a pod is unclaimed and StateClaimed once a
	// request has adopted it. Warm pods deliberately carry no sandbox-key label,
	// so the runtime's key-based Lookup/List/Events never surface them as
	// sandboxes until the moment of claim.
	LabelState = "hiver.sandbox.state"

	StateWarm    = "warm"
	StateClaimed = "claimed"
)

// WarmPool mirrors the warmpools.hiver.sh CRD (deployment/gke/k8s/warmpool-crd.yaml).
type WarmPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WarmPoolSpec   `json:"spec"`
	Status            WarmPoolStatus `json:"status,omitempty"`
}

// WarmPoolSpec is the desired state: a set of independent per-image warm buffers.
type WarmPoolSpec struct {
	// Images is the set of warm buffers to keep, one per image. Each entry carries
	// its own readyReplicas and maxReplicas, so a single pool can keep several
	// images warm with different sizing for each. A claim only hits a pod whose
	// image matches the requesting GetOrCreate.
	Images []WarmPoolImage `json:"images"`
}

// WarmPoolImage is one image's warm buffer: the image to pre-boot and how many
// to keep. The image is the claim key (SpecHash), because it is frozen at boot
// and can't be changed by the config a claim delivers — and it determines the
// isolation, which sandboxd derives from the image rather than from config.
type WarmPoolImage struct {
	// Image is the sandbox image to pre-boot.
	Image string `json:"image"`
	// ReadyReplicas is the target number of warm (unclaimed, ready) pods to keep
	// for this image.
	ReadyReplicas int `json:"readyReplicas"`
	// MaxReplicas caps total pods for this image (warm + claimed); the warm buffer
	// shrinks as claims grow so warm+claimed never exceeds it.
	MaxReplicas int `json:"maxReplicas"`
}

// WarmPoolStatus is the observed state the reconciler writes back.
type WarmPoolStatus struct {
	ReadyReplicas      int   `json:"readyReplicas"`
	ClaimedReplicas    int   `json:"claimedReplicas"`
	ObservedGeneration int64 `json:"observedGeneration"`
}

// WarmTarget returns how many warm pods to keep for this image right now given
// the number already claimed of it: ReadyReplicas, clamped so warm+claimed never
// exceeds MaxReplicas. Never negative.
func (i WarmPoolImage) WarmTarget(claimed int) int {
	headroom := i.MaxReplicas - claimed
	target := i.ReadyReplicas
	if target > headroom {
		target = headroom
	}
	if target < 0 {
		target = 0
	}
	return target
}

// SpecHash is the LabelSpec value for an image: the claim key shared by a warm
// pod and the requests that may adopt it. It is a truncated SHA-256 (hex,
// label-safe) of the image — image names contain "/" and ":" which aren't valid
// label values, so the raw image can't be a label.
func SpecHash(image string) string {
	sum := sha256.Sum256([]byte(image))
	return hex.EncodeToString(sum[:8])
}
