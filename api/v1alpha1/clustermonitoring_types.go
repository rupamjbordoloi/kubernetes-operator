/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Platform is the Kubernetes distribution the cluster runs on.
// +kubebuilder:validation:Enum=gke;eks;ocp
type Platform string

const (
	PlatformGKE Platform = "gke"
	PlatformEKS Platform = "eks"
	PlatformOCP Platform = "ocp"
)

// Condition types and reasons written to status.conditions.
const (
	ConditionReady = "Ready"

	ReasonAllComponentsRunning = "AllComponentsRunning"
	ReasonProgressing          = "Progressing"
	ReasonReconcileError       = "ReconcileError"
	ReasonDuplicate            = "DuplicateClusterMonitoring"
)

// ClusterMonitoringSpec defines the desired state of ClusterMonitoring.
type ClusterMonitoringSpec struct {
	// platform is the Kubernetes distribution. Each platform needs slightly
	// different collector settings (e.g. OpenShift needs extra SCC permissions).
	// +required
	Platform Platform `json:"platform"`

	// clusterName is added as the k8s.cluster.name label on every metric and
	// log so clusters can be told apart in the backend.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	ClusterName string `json:"clusterName"`

	// environment (prod, uat, dev, ...) is added as the deployment.environment
	// label. Defaults to the operator's --default-environment flag.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Environment string `json:"environment,omitempty"`

	// collect selects which signals are collected.
	// +optional
	// +kubebuilder:default={}
	Collect CollectSpec `json:"collect,omitzero"`

	// collectionInterval is how often metrics are scraped. Default: 30s.
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Format=duration
	CollectionInterval *metav1.Duration `json:"collectionInterval,omitempty"`

	// destination overrides where data is sent. Any endpoint left empty falls
	// back to the backend configured when the operator was installed.
	// +optional
	Destination *DestinationSpec `json:"destination,omitempty"`

	// collectorResources overrides CPU / memory of the collector pods.
	// Default: sized by the operator per platform.
	// +optional
	CollectorResources *CollectorResourcesSpec `json:"collectorResources,omitempty"`
}

// CollectSpec toggles each signal. Pointers are used so an explicit `false`
// is never confused with "not set".
type CollectSpec struct {
	// nodes collects CPU, memory and disk of each node.
	// +optional
	// +kubebuilder:default=true
	Nodes *bool `json:"nodes,omitempty"`

	// pods collects CPU, memory and restarts of each pod.
	// +optional
	// +kubebuilder:default=true
	Pods *bool `json:"pods,omitempty"`

	// events collects Kubernetes events ("pod crashed", "image pull failed").
	// +optional
	// +kubebuilder:default=true
	Events *bool `json:"events,omitempty"`

	// logs collects container logs from every node. Opt-in: high volume.
	// +optional
	// +kubebuilder:default=false
	Logs *bool `json:"logs,omitempty"`
}

// DestinationSpec holds OTLP/HTTP base URLs per signal.
type DestinationSpec struct {
	// metrics OTLP/HTTP endpoint, e.g. http://mimir.observability.svc:9009/otlp
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Metrics string `json:"metrics,omitempty"`

	// traces OTLP/HTTP endpoint, e.g. http://tempo.observability.svc:4318
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Traces string `json:"traces,omitempty"`

	// logs OTLP/HTTP endpoint, e.g. http://loki.observability.svc:3100/otlp
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Logs string `json:"logs,omitempty"`
}

// CollectorResourcesSpec sizes the two collector workloads.
type CollectorResourcesSpec struct {
	// perNode sizes the DaemonSet collector (one pod per node).
	// +optional
	PerNode *ResourceSize `json:"perNode,omitempty"`

	// central sizes the Deployment collector (cluster metrics, events, gateway).
	// +optional
	Central *ResourceSize `json:"central,omitempty"`
}

// ResourceSize is a CPU / memory pair used as both request and limit.
type ResourceSize struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// ClusterMonitoringStatus defines the observed state of ClusterMonitoring.
type ClusterMonitoringStatus struct {
	// observedGeneration is the spec generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the ClusterMonitoring resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cmon
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platform`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterMonitoring is the Schema for the clustermonitorings API.
// One per cluster: it tells Drishti to monitor the whole cluster.
type ClusterMonitoring struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterMonitoring
	// +required
	Spec ClusterMonitoringSpec `json:"spec"`

	// status defines the observed state of ClusterMonitoring
	// +optional
	Status ClusterMonitoringStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterMonitoringList contains a list of ClusterMonitoring
type ClusterMonitoringList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterMonitoring `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterMonitoring{}, &ClusterMonitoringList{})
		return nil
	})
}

// The helpers below read the spec with defaults applied, so the controller
// behaves the same even if an object skipped API-server defaulting.

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func (c CollectSpec) NodesEnabled() bool  { return boolOr(c.Nodes, true) }
func (c CollectSpec) PodsEnabled() bool   { return boolOr(c.Pods, true) }
func (c CollectSpec) EventsEnabled() bool { return boolOr(c.Events, true) }
func (c CollectSpec) LogsEnabled() bool   { return boolOr(c.Logs, false) }

// Interval returns collectionInterval, defaulting to 30s.
func (s ClusterMonitoringSpec) Interval() time.Duration {
	if s.CollectionInterval == nil || s.CollectionInterval.Duration <= 0 {
		return 30 * time.Second
	}
	return s.CollectionInterval.Duration
}
