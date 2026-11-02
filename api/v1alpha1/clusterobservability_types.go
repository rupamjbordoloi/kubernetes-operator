package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ClusterObservabilitySpec struct {
	// Enables or disables cluster monitoring.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Controls log collection.
	// +optional
	Logs TelemetryConfig `json:"logs,omitempty"`

	// Controls metric collection.
	// +optional
	Metrics TelemetryConfig `json:"metrics,omitempty"`
}

type TelemetryConfig struct {
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ClusterObservability struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterObservabilitySpec `json:"spec,omitempty"`

	Status ClusterObservabilityStatus `json:"status,omitempty"`
}

type ClusterObservabilityStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterObservabilityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterObservability `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		func(scheme *runtime.Scheme) error {
			scheme.AddKnownTypes(
				GroupVersion,
				&ClusterObservability{},
				&ClusterObservabilityList{},
			)

			metav1.AddToGroupVersion(scheme, GroupVersion)

			return nil
		},
	)
}
