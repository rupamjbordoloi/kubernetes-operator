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
	"fmt"

	"sigs.k8s.io/yaml"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

const (
	otlpGRPCPort = 4317
	otlpHTTPPort = 4318
	healthPort   = 13133

	configFileName = "config.yaml"
)

// m keeps the collector config literals readable.
type m = map[string]any

// agentEnabled reports whether the per-node DaemonSet is needed at all.
func agentEnabled(cm *monitoringv1alpha1.ClusterMonitoring) bool {
	c := cm.Spec.Collect
	return c.NodesEnabled() || c.PodsEnabled() || c.LogsEnabled()
}

func pipeline(receivers, processors, exporters []string) m {
	return m{"receivers": receivers, "processors": processors, "exporters": exporters}
}

// baseConfig holds the pieces shared by both collectors.
func baseConfig() m {
	return m{
		"extensions": m{
			"health_check": m{"endpoint": fmt.Sprintf("0.0.0.0:%d", healthPort)},
		},
		"processors": m{
			// Uses the container memory limit, so the pod sheds load before OOM.
			"memory_limiter": m{
				"check_interval":         "1s",
				"limit_percentage":       80,
				"spike_limit_percentage": 25,
			},
			"batch": m{"send_batch_size": 8192, "timeout": "5s"},
		},
	}
}

// renderAgentConfig builds the DaemonSet collector config. It reads node and
// pod stats from the local kubelet and (optionally) tails container logs, then
// forwards everything to the gateway over OTLP/gRPC.
func renderAgentConfig(cm *monitoringv1alpha1.ClusterMonitoring, gatewayEndpoint, namespace string) (string, error) {
	c := cm.Spec.Collect
	cfg := baseConfig()
	receivers := m{}
	pipelines := m{}

	processors := cfg["processors"].(m)
	processors["k8sattributes"] = m{
		"auth_type":   "serviceAccount",
		"passthrough": false,
		// Only watch pods on this node: keeps memory flat on large clusters.
		"filter": m{"node_from_env_var": "K8S_NODE_NAME"},
		"extract": m{
			"metadata": []string{
				"k8s.namespace.name", "k8s.pod.name", "k8s.pod.uid", "k8s.node.name",
				"k8s.deployment.name", "k8s.statefulset.name", "k8s.daemonset.name",
				"k8s.job.name", "k8s.cronjob.name",
			},
		},
		"pod_association": []any{
			m{"sources": []any{m{"from": "resource_attribute", "name": "k8s.pod.uid"}}},
			m{"sources": []any{m{"from": "connection"}}},
		},
	}

	exporters := m{
		"otlp/gateway": m{"endpoint": gatewayEndpoint, "tls": m{"insecure": true}},
	}
	agentProcessors := []string{"memory_limiter", "k8sattributes", "batch"}

	if c.NodesEnabled() || c.PodsEnabled() {
		var groups []string
		if c.NodesEnabled() {
			groups = append(groups, "node")
		}
		if c.PodsEnabled() {
			groups = append(groups, "pod", "container")
		}
		receivers["kubeletstats"] = m{
			"collection_interval": cm.Spec.Interval().String(),
			"auth_type":           "serviceAccount",
			// hostIP works on every platform; kubelet serving certs are often
			// self-signed (GKE, kind), hence insecure_skip_verify.
			"endpoint":             "https://${env:K8S_NODE_IP}:10250",
			"insecure_skip_verify": true,
			"metric_groups":        groups,
			// Use the *.cpu.usage metrics; the *.cpu.utilization ones are
			// deprecated but still on by default and log a warning.
			"metrics": m{
				"k8s.node.cpu.usage":        m{"enabled": true},
				"k8s.pod.cpu.usage":         m{"enabled": true},
				"container.cpu.usage":       m{"enabled": true},
				"k8s.node.cpu.utilization":  m{"enabled": false},
				"k8s.pod.cpu.utilization":   m{"enabled": false},
				"container.cpu.utilization": m{"enabled": false},
			},
		}
		pipelines["metrics"] = pipeline([]string{"kubeletstats"}, agentProcessors, []string{"otlp/gateway"})
	}

	if c.LogsEnabled() {
		receivers["filelog"] = m{
			"include": []string{"/var/log/pods/*/*/*.log"},
			// Never collect our own logs: it would feed back on itself.
			"exclude":           []string{fmt.Sprintf("/var/log/pods/%s_%s-*/*/*.log", namespace, agentName(cm))},
			"start_at":          "end",
			"include_file_path": true,
			"include_file_name": false,
			"operators": []any{
				// Parses docker / containerd / cri-o formats and sets
				// k8s.namespace.name, k8s.pod.name, k8s.pod.uid, k8s.container.name.
				m{"type": "container", "id": "container-parser"},
			},
		}
		pipelines["logs"] = pipeline([]string{"filelog"}, agentProcessors, []string{"otlp/gateway"})
	}

	cfg["receivers"] = receivers
	cfg["exporters"] = exporters
	cfg["service"] = m{"extensions": []string{"health_check"}, "pipelines": pipelines}
	return marshal(cfg)
}

// renderGatewayConfig builds the Deployment collector config. It is the only
// component that talks to the backend: it receives OTLP from the agents (and
// from apps), adds cluster-level metrics and events, stamps the cluster /
// environment labels, and exports.
func renderGatewayConfig(cm *monitoringv1alpha1.ClusterMonitoring, environment string,
	dest monitoringv1alpha1.DestinationSpec) (string, error) {
	c := cm.Spec.Collect
	cfg := baseConfig()

	receivers := m{
		"otlp": m{"protocols": m{
			"grpc": m{"endpoint": fmt.Sprintf("0.0.0.0:%d", otlpGRPCPort)},
			"http": m{"endpoint": fmt.Sprintf("0.0.0.0:%d", otlpHTTPPort)},
		}},
	}
	metricsReceivers := []string{"otlp"}
	logsReceivers := []string{"otlp"}

	if c.NodesEnabled() || c.PodsEnabled() {
		k8sCluster := m{
			"auth_type":                 "serviceAccount",
			"collection_interval":       cm.Spec.Interval().String(),
			"node_conditions_to_report": []string{"Ready", "MemoryPressure", "DiskPressure", "PIDPressure"},
		}
		if cm.Spec.Platform == monitoringv1alpha1.PlatformOCP {
			// Adds ClusterResourceQuota metrics.
			k8sCluster["distribution"] = "openshift"
		}
		receivers["k8s_cluster"] = k8sCluster
		metricsReceivers = append(metricsReceivers, "k8s_cluster")
	}
	if c.EventsEnabled() {
		receivers["k8sobjects"] = m{
			"auth_type": "serviceAccount",
			"objects": []any{
				m{"name": "events", "mode": "watch", "group": "events.k8s.io"},
			},
		}
		logsReceivers = append(logsReceivers, "k8sobjects")
	}

	processors := cfg["processors"].(m)
	processors["resource/cluster"] = m{"attributes": []any{
		m{"key": "k8s.cluster.name", "value": cm.Spec.ClusterName, "action": "upsert"},
		m{"key": "deployment.environment", "value": environment, "action": "upsert"},
	}}
	// Prometheus-style backends keep resource attributes out of series labels
	// by default. Copy the two we filter on onto every datapoint so queries can
	// filter by them without backend-side configuration.
	processors["transform/labels"] = m{
		"error_mode": "ignore",
		"metric_statements": []any{m{
			"context": "datapoint",
			"statements": []string{
				`set(attributes["k8s.cluster.name"], resource.attributes["k8s.cluster.name"])`,
				`set(attributes["deployment.environment"], resource.attributes["deployment.environment"])`,
			},
		}},
	}

	exporters := m{}
	exporterFor := func(signal, endpoint string) string {
		if endpoint == "" {
			// No backend configured anywhere: print to stdout so the
			// pipeline is still valid and can be inspected with kubectl logs.
			exporters["debug"] = m{"verbosity": "basic"}
			return "debug"
		}
		name := "otlphttp/" + signal
		exporters[name] = m{"endpoint": endpoint}
		return name
	}

	common := []string{"memory_limiter", "resource/cluster", "batch"}
	pipelines := m{
		"metrics": pipeline(metricsReceivers,
			[]string{"memory_limiter", "resource/cluster", "transform/labels", "batch"},
			[]string{exporterFor("metrics", dest.Metrics)}),
		"logs":   pipeline(logsReceivers, common, []string{exporterFor("logs", dest.Logs)}),
		"traces": pipeline([]string{"otlp"}, common, []string{exporterFor("traces", dest.Traces)}),
	}

	cfg["receivers"] = receivers
	cfg["exporters"] = exporters
	cfg["service"] = m{"extensions": []string{"health_check"}, "pipelines": pipelines}
	return marshal(cfg)
}

func marshal(cfg m) (string, error) {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal collector config: %w", err)
	}
	return string(out), nil
}
