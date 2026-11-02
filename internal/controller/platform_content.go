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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

// Metric names below follow the Prometheus translation of OTel metrics that
// Mimir / Prometheus apply on OTLP ingest: dots become underscores and units
// become suffixes (k8s.node.memory.working_set -> k8s_node_memory_working_set_bytes).
// The gateway copies k8s.cluster.name onto every datapoint, so it is
// available as the k8s_cluster_name label.

const (
	// grafanaDashboardLabel is what the Grafana Helm chart's dashboard
	// sidecar watches for by default.
	grafanaDashboardLabel = "grafana_dashboard"
)

// PrometheusRuleGVK is the prometheus-operator CRD used for default alerts.
// It is optional: when the CRD is not installed, alerts are skipped.
var PrometheusRuleGVK = schema.GroupVersionKind{
	Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule",
}

// ---- dashboard ----------------------------------------------------------------

func panel(id int, title, expr, legend, unit string, x, y int) m {
	return m{
		"id":         id,
		"type":       "timeseries",
		"title":      title,
		"datasource": m{"type": "prometheus", "uid": "${datasource}"},
		"gridPos":    m{"h": 8, "w": 12, "x": x, "y": y},
		"fieldConfig": m{
			"defaults":  m{"unit": unit},
			"overrides": []any{},
		},
		"targets": []any{m{"refId": "A", "expr": expr, "legendFormat": legend}},
	}
}

func renderDashboard(cm *monitoringv1alpha1.ClusterMonitoring) (string, error) {
	sel := `k8s_cluster_name="$cluster"`
	panels := []any{
		panel(1, "Node CPU (cores)", fmt.Sprintf(`sum by (k8s_node_name) (k8s_node_cpu_usage{%s})`, sel),
			"{{k8s_node_name}}", "short", 0, 0),
		panel(2, "Node memory working set", fmt.Sprintf(`sum by (k8s_node_name) (k8s_node_memory_working_set_bytes{%s})`, sel),
			"{{k8s_node_name}}", "bytes", 12, 0),
		panel(3, "Top 10 pods by CPU", fmt.Sprintf(`topk(10, sum by (k8s_namespace_name, k8s_pod_name) (k8s_pod_cpu_usage{%s}))`, sel),
			"{{k8s_namespace_name}}/{{k8s_pod_name}}", "short", 0, 8),
		panel(4, "Top 10 pods by memory", fmt.Sprintf(`topk(10, sum by (k8s_namespace_name, k8s_pod_name) (k8s_pod_memory_working_set_bytes{%s}))`, sel),
			"{{k8s_namespace_name}}/{{k8s_pod_name}}", "bytes", 12, 8),
		panel(5, "Container restarts (15m)", fmt.Sprintf(`sum by (k8s_namespace_name, k8s_pod_name) (delta(k8s_container_restarts{%s}[15m])) > 0`, sel),
			"{{k8s_namespace_name}}/{{k8s_pod_name}}", "short", 0, 16),
		panel(6, "Node filesystem free %", fmt.Sprintf(`100 * k8s_node_filesystem_available_bytes{%[1]s} / k8s_node_filesystem_capacity_bytes{%[1]s}`, sel),
			"{{k8s_node_name}}", "percent", 12, 16),
	}

	dashboard := m{
		"uid":           hash("drishti-platform-" + cm.Spec.ClusterName),
		"title":         "Drishti / Platform / " + cm.Spec.ClusterName,
		"tags":          []string{"drishti", "platform"},
		"schemaVersion": 39,
		"refresh":       "30s",
		"time":          m{"from": "now-6h", "to": "now"},
		"templating": m{"list": []any{
			m{"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Data source"},
			m{
				"name": "cluster", "type": "query", "label": "Cluster",
				"datasource": m{"type": "prometheus", "uid": "${datasource}"},
				"query":      "label_values(k8s_node_memory_working_set_bytes, k8s_cluster_name)",
				"current":    m{"text": cm.Spec.ClusterName, "value": cm.Spec.ClusterName},
				"refresh":    2,
			},
		}},
		"panels": panels,
	}
	out, err := json.MarshalIndent(dashboard, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal dashboard: %w", err)
	}
	return string(out), nil
}

func mutateDashboardConfigMap(cm *monitoringv1alpha1.ClusterMonitoring, obj *corev1.ConfigMap, dashboard string) {
	labels := commonLabels(cm)
	labels[grafanaDashboardLabel] = "1"
	obj.Labels = labels
	obj.Data = map[string]string{"drishti-platform-" + cm.Spec.ClusterName + ".json": dashboard}
}

// ---- alerts -------------------------------------------------------------------

type alertRule struct {
	name, expr, forDur, severity, summary string
}

func platformAlertRules(cm *monitoringv1alpha1.ClusterMonitoring) []alertRule {
	sel := fmt.Sprintf(`k8s_cluster_name=%q`, cm.Spec.ClusterName)
	return []alertRule{
		{"DrishtiNodeNotReady",
			fmt.Sprintf(`k8s_node_condition_ready{%s} == 0`, sel),
			"5m", "critical", "Node {{ $labels.k8s_node_name }} is not Ready"},
		{"DrishtiNodeMemoryPressure",
			fmt.Sprintf(`k8s_node_memory_available_bytes{%[1]s} / (k8s_node_memory_available_bytes{%[1]s} + k8s_node_memory_working_set_bytes{%[1]s}) < 0.10`, sel),
			"10m", "warning", "Node {{ $labels.k8s_node_name }} has less than 10% memory available"},
		{"DrishtiNodeFilesystemAlmostFull",
			fmt.Sprintf(`k8s_node_filesystem_available_bytes{%[1]s} / k8s_node_filesystem_capacity_bytes{%[1]s} < 0.10`, sel),
			"15m", "warning", "Node {{ $labels.k8s_node_name }} filesystem is more than 90% full"},
		{"DrishtiPodCrashLooping",
			fmt.Sprintf(`delta(k8s_container_restarts{%s}[15m]) > 3`, sel),
			"0m", "warning", "{{ $labels.k8s_namespace_name }}/{{ $labels.k8s_pod_name }} restarted more than 3 times in 15m"},
		{"DrishtiPodPendingTooLong",
			// k8s.pod.phase: 1=Pending 2=Running 3=Succeeded 4=Failed 5=Unknown
			fmt.Sprintf(`k8s_pod_phase{%s} == 1`, sel),
			"15m", "warning", "{{ $labels.k8s_namespace_name }}/{{ $labels.k8s_pod_name }} has been Pending for 15m"},
		{"DrishtiDeploymentReplicasUnavailable",
			fmt.Sprintf(`k8s_deployment_desired{%[1]s} - k8s_deployment_available{%[1]s} > 0`, sel),
			"15m", "warning", "Deployment {{ $labels.k8s_namespace_name }}/{{ $labels.k8s_deployment_name }} is missing replicas"},
	}
}

func mutatePrometheusRule(cm *monitoringv1alpha1.ClusterMonitoring, obj *unstructured.Unstructured,
	environment string) error {
	rules := make([]any, 0, len(platformAlertRules(cm)))
	for _, r := range platformAlertRules(cm) {
		rules = append(rules, map[string]any{
			"alert": r.name,
			"expr":  r.expr,
			"for":   r.forDur,
			"labels": map[string]any{
				"severity":    r.severity,
				"cluster":     cm.Spec.ClusterName,
				"environment": environment,
				"source":      "drishti",
			},
			"annotations": map[string]any{"summary": r.summary},
		})
	}
	obj.SetLabels(commonLabels(cm))
	return unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"name": "drishti-platform", "rules": rules},
	}, "spec", "groups")
}
