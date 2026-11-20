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

	// eventsServiceName is the service.name given to Kubernetes events.
	eventsServiceName = "kubernetes-events"
)

// Collector component IDs. Pipelines refer to components by these IDs, so a
// typo would only show up when the collector refuses to start.
const (
	otlpReceiver    = "otlp"
	memoryLimiter   = "memory_limiter"
	batchProcessor  = "batch"
	gatewayExporter = "otlp_grpc/gateway"
	// backendExporter is the prefix of the gateway's per-signal exporters,
	// e.g. otlp_http/metrics.
	backendExporter = "otlp_http/"
	// clusterLabels stamps k8s.cluster.name and deployment.environment.
	clusterLabels = "resource/cluster"

	// Contrib components, under the names used since collector 0.16x (the
	// old kubeletstats / filelog / k8sobjects / k8sattributes still work but
	// log a deprecation warning).
	kubeletStatsReceiver   = "kubelet_stats"
	k8sClusterReceiver     = "k8s_cluster"
	fileLogReceiver        = "file_log"
	k8sObjectsReceiver     = "k8s_objects"
	k8sAttributesProcessor = "k8s_attributes"
)

// Config keys used by several components.
const (
	keyEndpoint = "endpoint"
	keyAuthType = "auth_type"
	// serviceAccountAuth makes a component call the API server or kubelet
	// with the pod's service account token.
	serviceAccountAuth = "serviceAccount"
)

// m keeps the collector config literals readable.
type m = map[string]any

// agentEnabled reports whether the per-node DaemonSet is needed at all.
func agentEnabled(cm *monitoringv1alpha1.ClusterMonitoring) bool {
	c := cm.Spec.Collect
	return c.NodesEnabled() || c.PodsEnabled() || c.LogsEnabled()
}

// clusterCollectorEnabled reports whether the cluster collector is needed:
// it reads cluster state (with nodes or pods) and events from the API server.
func clusterCollectorEnabled(cm *monitoringv1alpha1.ClusterMonitoring) bool {
	c := cm.Spec.Collect
	return c.NodesEnabled() || c.PodsEnabled() || c.EventsEnabled()
}

// toGateway is the exporter both the agents and the cluster collector use.
// The endpoint is a headless Service resolved with dns:///, and round_robin
// spreads requests over every gateway replica. A ClusterIP Service would pin
// each long-lived gRPC connection to a single replica.
func toGateway(endpoint string) m {
	return m{gatewayExporter: m{
		keyEndpoint:     endpoint,
		"balancer_name": "round_robin",
		"tls":           m{"insecure": true},
	}}
}

func pipeline(receivers, processors, exporters []string) m {
	return m{"receivers": receivers, "processors": processors, "exporters": exporters}
}

// service is the top-level "service" section: the health check plus pipelines.
func service(pipelines m) m {
	return m{"extensions": []string{"health_check"}, "pipelines": pipelines}
}

// attribute is one entry of a resource processor: set key to value, where
// action is "insert" (only if missing) or "upsert" (always).
func attribute(key, value, action string) m {
	return m{"key": key, "value": value, "action": action}
}

// baseConfig holds the pieces shared by all three collectors.
func baseConfig() m {
	return m{
		"extensions": m{
			"health_check": m{keyEndpoint: fmt.Sprintf("0.0.0.0:%d", healthPort)},
		},
		"processors": m{
			// Uses the container memory limit, so the pod sheds load before OOM.
			memoryLimiter: m{
				"check_interval":         "1s",
				"limit_percentage":       80,
				"spike_limit_percentage": 25,
			},
			batchProcessor: m{"send_batch_size": 8192, "timeout": "5s"},
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
	processors[k8sAttributesProcessor] = m{
		keyAuthType:   serviceAccountAuth,
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

	exporters := toGateway(gatewayEndpoint)
	agentProcessors := []string{memoryLimiter, k8sAttributesProcessor, batchProcessor}

	if c.NodesEnabled() || c.PodsEnabled() {
		var groups []string
		if c.NodesEnabled() {
			groups = append(groups, "node")
		}
		if c.PodsEnabled() {
			groups = append(groups, "pod", "container")
		}
		receivers[kubeletStatsReceiver] = m{
			"collection_interval": cm.Spec.Interval().String(),
			keyAuthType:           serviceAccountAuth,
			// hostIP works on every platform; kubelet serving certs are often
			// self-signed (GKE, kind), hence insecure_skip_verify.
			keyEndpoint:            "https://${env:K8S_NODE_IP}:10250",
			"insecure_skip_verify": true,
			"metric_groups":        groups,
		}
		pipelines["metrics"] = pipeline([]string{kubeletStatsReceiver}, agentProcessors, []string{gatewayExporter})
	}

	if c.LogsEnabled() {
		receivers[fileLogReceiver] = m{
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
		pipelines["logs"] = pipeline([]string{fileLogReceiver}, agentProcessors, []string{gatewayExporter})
	}

	cfg["receivers"] = receivers
	cfg["exporters"] = exporters
	cfg["service"] = service(pipelines)
	return marshal(cfg)
}

// renderClusterCollectorConfig builds the config of the single cluster
// collector. It is the only component that reads cluster-wide state from the
// API server (k8s_cluster metrics and k8s_objects events), so it must run
// exactly once per cluster. It forwards everything to the gateway.
func renderClusterCollectorConfig(cm *monitoringv1alpha1.ClusterMonitoring, gatewayEndpoint string) (string, error) {
	c := cm.Spec.Collect
	cfg := baseConfig()
	receivers := m{}
	pipelines := m{}
	processors := cfg["processors"].(m)

	if c.NodesEnabled() || c.PodsEnabled() {
		k8sCluster := m{
			keyAuthType:                 serviceAccountAuth,
			"collection_interval":       cm.Spec.Interval().String(),
			"node_conditions_to_report": []string{"Ready", "MemoryPressure", "DiskPressure", "PIDPressure"},
		}
		if cm.Spec.Platform == monitoringv1alpha1.PlatformOCP {
			// Adds ClusterResourceQuota metrics.
			k8sCluster["distribution"] = "openshift"
		}
		receivers[k8sClusterReceiver] = k8sCluster
		pipelines["metrics"] = pipeline([]string{k8sClusterReceiver},
			[]string{memoryLimiter, batchProcessor}, []string{gatewayExporter})
	}
	if c.EventsEnabled() {
		receivers[k8sObjectsReceiver] = m{
			keyAuthType: serviceAccountAuth,
			"objects": []any{
				m{
					"name": eventsResource, "mode": "watch", "group": "events.k8s.io",
					// DELETED is Kubernetes expiring an old Event (about 1h TTL),
					// not something that happened in the cluster.
					"exclude_watch_type": []string{"DELETED"},
				},
			},
		}
		// Events carry no service attributes, so Loki would file them under
		// unknown_service. Only fill it in: never override a real service.name.
		processors["resource/events"] = m{"attributes": []any{
			attribute("service.name", eventsServiceName, "insert"),
		}}
		// managedFields is API server bookkeeping and most of each record.
		processors["transform/events"] = m{
			"error_mode": "ignore",
			"log_statements": []any{m{
				"context":    "log",
				"statements": []string{`delete_key(body["object"]["metadata"], "managedFields")`},
			}},
		}
		pipelines["logs/events"] = pipeline([]string{k8sObjectsReceiver},
			[]string{memoryLimiter, "resource/events", "transform/events", batchProcessor},
			[]string{gatewayExporter})
	}

	cfg["receivers"] = receivers
	cfg["exporters"] = toGateway(gatewayEndpoint)
	cfg["service"] = service(pipelines)
	return marshal(cfg)
}

// renderGatewayConfig builds the gateway config. It is the only component
// that talks to the backends: it receives OTLP metrics and logs from the
// agents and the cluster collector, stamps the cluster / environment labels,
// and exports.
// It holds no cluster state, so any number of replicas can run it.
func renderGatewayConfig(cm *monitoringv1alpha1.ClusterMonitoring, environment string,
	dest monitoringv1alpha1.DestinationSpec) (string, error) {
	cfg := baseConfig()
	processors := cfg["processors"].(m)

	receivers := m{
		otlpReceiver: m{"protocols": m{
			"grpc": m{keyEndpoint: fmt.Sprintf("0.0.0.0:%d", otlpGRPCPort)},
			"http": m{keyEndpoint: fmt.Sprintf("0.0.0.0:%d", otlpHTTPPort)},
		}},
	}

	processors[clusterLabels] = m{"attributes": []any{
		attribute("k8s.cluster.name", cm.Spec.ClusterName, "upsert"),
		attribute("deployment.environment", environment, "upsert"),
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
		name := backendExporter + signal
		exporters[name] = m{keyEndpoint: endpoint}
		return name
	}

	otlpIn := []string{otlpReceiver}
	pipelines := m{
		"metrics": pipeline(otlpIn,
			[]string{memoryLimiter, clusterLabels, "transform/labels", batchProcessor},
			[]string{exporterFor("metrics", dest.Metrics)}),
		"logs": pipeline(otlpIn,
			[]string{memoryLimiter, clusterLabels, batchProcessor},
			[]string{exporterFor("logs", dest.Logs)}),
	}

	cfg["receivers"] = receivers
	cfg["exporters"] = exporters
	cfg["service"] = service(pipelines)
	return marshal(cfg)
}

func marshal(cfg m) (string, error) {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal collector config: %w", err)
	}
	return string(out), nil
}
