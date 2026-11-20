package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

func sampleCM(collect monitoringv1alpha1.CollectSpec) *monitoringv1alpha1.ClusterMonitoring {
	return &monitoringv1alpha1.ClusterMonitoring{
		ObjectMeta: metav1.ObjectMeta{Name: "hlr-gke-prod"},
		Spec: monitoringv1alpha1.ClusterMonitoringSpec{
			Platform:    monitoringv1alpha1.PlatformGKE,
			ClusterName: "hlr-gke-prod",
			Collect:     collect,
		},
	}
}

func pipelinesOf(cfg string) map[string]any {
	parsed := map[string]any{}
	Expect(yaml.Unmarshal([]byte(cfg), &parsed)).To(Succeed())
	return parsed["service"].(map[string]any)["pipelines"].(map[string]any)
}

var _ = Describe("Collector config rendering", func() {
	It("renders an agent with only a metrics pipeline by default", func() {
		cfg, err := renderAgentConfig(sampleCM(monitoringv1alpha1.CollectSpec{}), "gw:4317", "drishti-system")
		Expect(err).NotTo(HaveOccurred())
		p := pipelinesOf(cfg)
		Expect(p).To(HaveKey("metrics"))
		Expect(p).NotTo(HaveKey("logs"))
		Expect(cfg).To(ContainSubstring("collection_interval: 30s"))
		Expect(p["metrics"]).To(HaveKeyWithValue("exporters", ConsistOf(gatewayExporter)))
		// Removed from kubelet_stats in newer collectors: naming one fails startup.
		Expect(cfg).NotTo(ContainSubstring("cpu.utilization"))
	})

	It("adds a logs pipeline and excludes the agent's own logs when logs are on", func() {
		cfg, err := renderAgentConfig(sampleCM(monitoringv1alpha1.CollectSpec{Logs: new(true)}),
			"gw:4317", "drishti-system")
		Expect(err).NotTo(HaveOccurred())
		Expect(pipelinesOf(cfg)).To(HaveKey("logs"))
		Expect(cfg).To(ContainSubstring("/var/log/pods/drishti-system_hlr-gke-prod-agent-*/*/*.log"))
	})

	It("only asks kubelet for node stats when pods are off", func() {
		cfg, err := renderAgentConfig(sampleCM(monitoringv1alpha1.CollectSpec{Pods: new(false)}),
			"gw:4317", "drishti-system")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).To(ContainSubstring("- node"))
		Expect(cfg).NotTo(ContainSubstring("- container"))
	})

	It("falls back to the debug exporter when no destination is configured", func() {
		cfg, err := renderGatewayConfig(sampleCM(monitoringv1alpha1.CollectSpec{}), "prod",
			monitoringv1alpha1.DestinationSpec{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).To(ContainSubstring("debug:"))
		Expect(cfg).NotTo(ContainSubstring("otlp_http"))
		Expect(cfg).To(ContainSubstring("value: hlr-gke-prod"))
		Expect(cfg).To(ContainSubstring("value: prod"))
	})

	It("keeps the gateway free of cluster state, so it can scale out", func() {
		cfg, err := renderGatewayConfig(sampleCM(monitoringv1alpha1.CollectSpec{}), "prod",
			monitoringv1alpha1.DestinationSpec{Logs: "http://loki:3100/otlp"})
		Expect(err).NotTo(HaveOccurred())
		p := pipelinesOf(cfg)
		// Exactly two pipelines: metrics and logs.
		Expect(p).To(HaveLen(2))
		for _, name := range []string{"metrics", "logs"} {
			Expect(p[name]).To(HaveKeyWithValue("receivers", ConsistOf(otlpReceiver)))
		}
		Expect(cfg).NotTo(ContainSubstring(k8sClusterReceiver))
		Expect(cfg).NotTo(ContainSubstring(k8sObjectsReceiver))
		Expect(cfg).To(ContainSubstring("otlp_http/logs"))
	})

	It("sends cluster metrics and events from the cluster collector to the gateway", func() {
		cfg, err := renderClusterCollectorConfig(sampleCM(monitoringv1alpha1.CollectSpec{}), "dns:///gw:4317")
		Expect(err).NotTo(HaveOccurred())
		p := pipelinesOf(cfg)
		Expect(p["metrics"]).To(HaveKeyWithValue("receivers", ConsistOf(k8sClusterReceiver)))
		Expect(p["logs/events"]).To(HaveKeyWithValue("receivers", ConsistOf(k8sObjectsReceiver)))
		Expect(p["logs/events"]).To(HaveKeyWithValue("processors", ContainElement("resource/events")))
		for _, name := range []string{"metrics", "logs/events"} {
			Expect(p[name]).To(HaveKeyWithValue("exporters", ConsistOf(gatewayExporter)))
		}
		Expect(cfg).To(ContainSubstring("endpoint: dns:///gw:4317"))
		Expect(cfg).To(ContainSubstring("balancer_name: round_robin"))
		Expect(cfg).To(ContainSubstring("value: " + eventsServiceName))
		Expect(cfg).To(ContainSubstring("- DELETED"))
		Expect(cfg).To(ContainSubstring("managedFields"))
		// Only the gateway talks to the backends.
		Expect(cfg).NotTo(ContainSubstring(backendExporter))
	})

	It("drops the events pipeline from the cluster collector when events are off", func() {
		cfg, err := renderClusterCollectorConfig(sampleCM(monitoringv1alpha1.CollectSpec{Events: new(false)}),
			"dns:///gw:4317")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(ContainSubstring(k8sObjectsReceiver))
		Expect(cfg).NotTo(ContainSubstring("resource/events"))
		Expect(pipelinesOf(cfg)).NotTo(HaveKey("logs/events"))
		Expect(pipelinesOf(cfg)).To(HaveKey("metrics"))
	})

	It("only needs the cluster collector for cluster state or events", func() {
		off := new(false)
		Expect(clusterCollectorEnabled(sampleCM(monitoringv1alpha1.CollectSpec{}))).To(BeTrue())
		Expect(clusterCollectorEnabled(sampleCM(monitoringv1alpha1.CollectSpec{
			Nodes: off, Pods: off, Events: off, Logs: new(true),
		}))).To(BeFalse())
	})

	It("lets the CR destination override install defaults per signal", func() {
		o := Options{Destination: monitoringv1alpha1.DestinationSpec{Metrics: "http://default-m", Logs: "http://default-l"}}
		cm := sampleCM(monitoringv1alpha1.CollectSpec{})
		cm.Spec.Destination = &monitoringv1alpha1.DestinationSpec{Metrics: "http://override-m"}
		d := o.destination(cm)
		Expect(d.Metrics).To(Equal("http://override-m"))
		Expect(d.Logs).To(Equal("http://default-l"))
	})
})
