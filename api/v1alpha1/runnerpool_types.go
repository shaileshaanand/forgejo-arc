package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Scope struct {
	// +kubebuilder:validation:Enum=Repository
	Type string `json:"type"`
	// Name is owner/repository for repository scope.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type RunnerPoolSpec struct {
	// ForgejoURL includes any installation subpath.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="forgejoURL is immutable"
	ForgejoURL string `json:"forgejoURL"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="scope is immutable"
	Scope Scope `json:"scope"`
	// CredentialsSecret names a same-namespace Secret with a token key.
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret"`
	// Labels are the plain workflow runs-on labels, without backend suffixes.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Labels []string `json:"labels"`
	// MaxRunners includes workers which are still starting. Zero pauses new work.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxRunners *int32 `json:"maxRunners,omitempty"`
	// StartupTimeoutSeconds bounds waiting for a pod to acquire its job.
	// +kubebuilder:default=180
	// +kubebuilder:validation:Minimum=10
	StartupTimeoutSeconds int64 `json:"startupTimeoutSeconds,omitempty"`
	// MaxAttempts bounds infrastructure retries while Forgejo still reports waiting.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
	// +kubebuilder:default="data.forgejo.org/forgejo/runner:13.0.0"
	RunnerImage string `json:"runnerImage,omitempty"`
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`
	// JobTimeoutSeconds bounds the full Kubernetes worker lifetime, including startup.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=30
	JobTimeoutSeconds int64                       `json:"jobTimeoutSeconds,omitempty"`
	Resources         corev1.ResourceRequirements `json:"resources,omitempty"`
}

type RunnerPoolStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	ActiveRunners      int32 `json:"activeRunners"`
	WaitingJobs        int32 `json:"waitingJobs"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rp
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeRunners`
// +kubebuilder:printcolumn:name="Waiting",type=integer,JSONPath=`.status.waitingJobs`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type RunnerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RunnerPoolSpec   `json:"spec"`
	Status            RunnerPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RunnerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerPool `json:"items"`
}

func init() { SchemeBuilder.Register(&RunnerPool{}, &RunnerPoolList{}) }
