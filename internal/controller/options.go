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

package controller

import (
	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

const (
	// DefaultNamespace is where collectors run when the operator is started
	// outside the cluster (make run) and POD_NAMESPACE is not set.
	DefaultNamespace = "drishti-system"

	// DefaultCollectorImage is the OpenTelemetry Collector (contrib) image.
	// The contrib distribution is required for kubeletstats, k8s_cluster,
	// k8sobjects, k8sattributes and filelog.
	DefaultCollectorImage = "otel/opentelemetry-collector-contrib:0.120.0"

	// DefaultEnvironment is used when neither the CR nor the flag set one.
	DefaultEnvironment = "dev"
)

// Options are install-time settings (Helm values / manager flags) that apply
// to every ClusterMonitoring. The CR only overrides them when it has to.
type Options struct {
	// Namespace where the collector DaemonSet / Deployment are created.
	Namespace string
	// DashboardNamespace is where dashboard ConfigMaps go (Grafana sidecar
	// watches it). Defaults to Namespace.
	DashboardNamespace string
	// CollectorImage is the OTel collector image.
	CollectorImage string
	// Environment is the default deployment.environment label.
	Environment string
	// Destination holds the default backend endpoints.
	Destination monitoringv1alpha1.DestinationSpec
}

// withDefaults fills empty fields so the reconciler never sees blanks.
func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.DashboardNamespace == "" {
		o.DashboardNamespace = o.Namespace
	}
	if o.CollectorImage == "" {
		o.CollectorImage = DefaultCollectorImage
	}
	if o.Environment == "" {
		o.Environment = DefaultEnvironment
	}
	return o
}

// environment returns the effective environment for a ClusterMonitoring.
func (o Options) environment(cm *monitoringv1alpha1.ClusterMonitoring) string {
	if cm.Spec.Environment != "" {
		return cm.Spec.Environment
	}
	return o.Environment
}

// destination merges the CR's destination over the install-time defaults,
// field by field. An empty result for a signal means "use the debug exporter".
func (o Options) destination(cm *monitoringv1alpha1.ClusterMonitoring) monitoringv1alpha1.DestinationSpec {
	d := o.Destination
	if cm.Spec.Destination == nil {
		return d
	}
	if cm.Spec.Destination.Metrics != "" {
		d.Metrics = cm.Spec.Destination.Metrics
	}
	if cm.Spec.Destination.Traces != "" {
		d.Traces = cm.Spec.Destination.Traces
	}
	if cm.Spec.Destination.Logs != "" {
		d.Logs = cm.Spec.Destination.Logs
	}
	return d
}
