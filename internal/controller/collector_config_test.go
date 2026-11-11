package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
	})

	It("adds a logs pipeline and excludes the agent's own logs when logs are on", func() {
		cfg, err := renderAgentConfig(sampleCM(monitoringv1alpha1.CollectSpec{Logs: ptr.To(true)}),
			"gw:4317", "drishti-system")
		Expect(err).NotTo(HaveOccurred())
		Expect(pipelinesOf(cfg)).To(HaveKey("logs"))
		Expect(cfg).To(ContainSubstring("/var/log/pods/drishti-system_hlr-gke-prod-agent-*/*/*.log"))
	})

	It("only asks kubelet for node stats when pods are off", func() {
		cfg, err := renderAgentConfig(sampleCM(monitoringv1alpha1.CollectSpec{Pods: ptr.To(false)}),
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
		Expect(cfg).NotTo(ContainSubstring("otlphttp"))
		Expect(cfg).To(ContainSubstring("value: hlr-gke-prod"))
		Expect(cfg).To(ContainSubstring("value: prod"))
	})

	It("sends events through their own pipeline with a service name", func() {
		cfg, err := renderGatewayConfig(sampleCM(monitoringv1alpha1.CollectSpec{}), "prod",
			monitoringv1alpha1.DestinationSpec{Logs: "http://loki:3100/otlp"})
		Expect(err).NotTo(HaveOccurred())
		p := pipelinesOf(cfg)
		Expect(p["logs/events"]).To(HaveKeyWithValue("receivers", ConsistOf("k8sobjects")))
		Expect(p["logs/events"]).To(HaveKeyWithValue("exporters", ConsistOf("otlphttp/logs")))
		Expect(p["logs"]).To(HaveKeyWithValue("receivers", ConsistOf("otlp")))
		Expect(p["logs"]).To(HaveKeyWithValue("processors", Not(ContainElement("resource/events"))))
		Expect(cfg).To(ContainSubstring("value: " + eventsServiceName))
		Expect(cfg).To(ContainSubstring("- DELETED"))
		Expect(cfg).To(ContainSubstring("managedFields"))
	})

	It("drops the events receiver when events are off", func() {
		cfg, err := renderGatewayConfig(sampleCM(monitoringv1alpha1.CollectSpec{Events: ptr.To(false)}), "prod",
			monitoringv1alpha1.DestinationSpec{Logs: "http://loki:3100/otlp"})
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(ContainSubstring("k8sobjects"))
		Expect(cfg).NotTo(ContainSubstring("resource/events"))
		Expect(pipelinesOf(cfg)).NotTo(HaveKey("logs/events"))
		Expect(cfg).To(ContainSubstring("otlphttp/logs"))
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
