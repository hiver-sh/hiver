package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// warmPoolGVR identifies the WarmPool custom resource. The reconciler reads
// WarmPools through the dynamic client and decodes them into the typed structs
// below with runtime.DefaultUnstructuredConverter — so no generated clientset or
// scheme registration is needed, keeping the dependency surface to the client-go
// the controller already uses.
var warmPoolGVR = schema.GroupVersionResource{Group: "hiver.sh", Version: "v1alpha1", Resource: "warmpools"}

// WarmPool mirrors the warmpools.hiver.sh CRD (deployment/gke/k8s/warmpool-crd.yaml).
type WarmPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WarmPoolSpec   `json:"spec"`
	Status            WarmPoolStatus `json:"status,omitempty"`
}

// WarmPoolSpec is the desired state: how many pre-booted pods of Image to keep.
type WarmPoolSpec struct {
	// Image is the sandbox image to pre-boot. A claim only hits when the
	// requesting GetOrCreate uses the same image.
	Image string `json:"image"`
	// ReadyReplicas is the target number of warm (unclaimed, ready) pods.
	ReadyReplicas int `json:"readyReplicas"`
	// MaxReplicas caps total pods (warm + claimed); the warm buffer shrinks as
	// claims grow so warm+claimed never exceeds it.
	MaxReplicas int `json:"maxReplicas"`
}

// WarmPoolStatus is the observed state the reconciler writes back.
type WarmPoolStatus struct {
	ReadyReplicas      int   `json:"readyReplicas"`
	ClaimedReplicas    int   `json:"claimedReplicas"`
	ObservedGeneration int64 `json:"observedGeneration"`
}

// WarmTarget returns how many warm pods the pool should keep right now given the
// number already claimed: the ready buffer, clamped so warm+claimed never
// exceeds MaxReplicas. Never negative.
func (s WarmPoolSpec) WarmTarget(claimed int) int {
	headroom := s.MaxReplicas - claimed
	target := s.ReadyReplicas
	if target > headroom {
		target = headroom
	}
	if target < 0 {
		target = 0
	}
	return target
}
