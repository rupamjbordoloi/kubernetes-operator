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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

const (
	// ManagedByLabel marks every object the operator creates. The manager
	// cache is restricted to objects carrying it.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "drishti"

	componentAgent   = "agent"
	componentGateway = "gateway"

	configHashAnnotation = "drishti.io/config-hash"

	// openShiftPrivilegedSCCRole is the built-in OpenShift ClusterRole that
	// grants `use` on the privileged SCC (needed to read host log files).
	openShiftPrivilegedSCCRole = "system:openshift:scc:privileged"
)

// ---- names -----------------------------------------------------------------

func agentName(cm *monitoringv1alpha1.ClusterMonitoring) string   { return cm.Name + "-agent" }
func gatewayName(cm *monitoringv1alpha1.ClusterMonitoring) string { return cm.Name + "-gateway" }
func serviceAccountName(cm *monitoringv1alpha1.ClusterMonitoring) string {
	return cm.Name + "-collector"
}
func clusterRoleName(cm *monitoringv1alpha1.ClusterMonitoring) string {
	return "drishti:" + cm.Name + ":collector"
}
func sccBindingName(cm *monitoringv1alpha1.ClusterMonitoring) string {
	return "drishti:" + cm.Name + ":scc-privileged"
}

// gatewayEndpoint is the in-cluster OTLP/gRPC address agents export to.
func gatewayEndpoint(cm *monitoringv1alpha1.ClusterMonitoring, namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", gatewayName(cm), namespace, otlpGRPCPort)
}

// ---- labels ----------------------------------------------------------------

func commonLabels(cm *monitoringv1alpha1.ClusterMonitoring) map[string]string {
	return map[string]string{
		ManagedByLabel:               ManagedByValue,
		"app.kubernetes.io/instance": cm.Name,
		"app.kubernetes.io/part-of":  "drishti",
	}
}

func componentLabels(cm *monitoringv1alpha1.ClusterMonitoring, component string) map[string]string {
	l := commonLabels(cm)
	l["app.kubernetes.io/name"] = "otel-collector"
	l["app.kubernetes.io/component"] = component
	return l
}

// selectorLabels must never change after creation (selectors are immutable).
func selectorLabels(cm *monitoringv1alpha1.ClusterMonitoring, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance":  cm.Name,
		"app.kubernetes.io/component": component,
	}
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// ---- RBAC for the collectors ------------------------------------------------

// collectorRules is what kubeletstats, k8sattributes, k8s_cluster and
// k8sobjects need. The operator must hold every one of these itself
// (see the rbac markers in the controller) or the API server refuses to
// create the ClusterRole.
func collectorRules(cm *monitoringv1alpha1.ClusterMonitoring) []rbacv1.PolicyRule {
	read := []string{"get", "list", "watch"}
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Verbs: read, Resources: []string{
			"nodes", "nodes/stats", "nodes/proxy", "pods", "namespaces", "events", "services",
			"endpoints", "replicationcontrollers", "resourcequotas",
			"persistentvolumes", "persistentvolumeclaims",
		}},
		{APIGroups: []string{"apps"}, Verbs: read, Resources: []string{
			"daemonsets", "deployments", "replicasets", "statefulsets",
		}},
		{APIGroups: []string{"batch"}, Verbs: read, Resources: []string{"jobs", "cronjobs"}},
		{APIGroups: []string{"autoscaling"}, Verbs: read, Resources: []string{"horizontalpodautoscalers"}},
		{APIGroups: []string{"events.k8s.io"}, Verbs: read, Resources: []string{"events"}},
	}
	if cm.Spec.Platform == monitoringv1alpha1.PlatformOCP {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{"quota.openshift.io"}, Verbs: read, Resources: []string{"clusterresourcequotas"},
		})
	}
	return rules
}

func mutateClusterRole(cm *monitoringv1alpha1.ClusterMonitoring, cr *rbacv1.ClusterRole) {
	cr.Labels = commonLabels(cm)
	cr.Rules = collectorRules(cm)
}

func mutateClusterRoleBinding(cm *monitoringv1alpha1.ClusterMonitoring, crb *rbacv1.ClusterRoleBinding,
	roleName, namespace string) {
	crb.Labels = commonLabels(cm)
	crb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleName}
	crb.Subjects = []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName(cm), Namespace: namespace,
	}}
}

// ---- sizing ---------------------------------------------------------------

type size struct{ cpu, memory string }

// defaultSizes are per platform. OpenShift clusters usually run more pods per
// node, so the agent's k8sattributes cache gets a bit more memory.
var defaultSizes = map[monitoringv1alpha1.Platform]struct{ perNode, central size }{
	monitoringv1alpha1.PlatformGKE: {size{"200m", "256Mi"}, size{"500m", "1Gi"}},
	monitoringv1alpha1.PlatformEKS: {size{"200m", "256Mi"}, size{"500m", "1Gi"}},
	monitoringv1alpha1.PlatformOCP: {size{"250m", "384Mi"}, size{"500m", "1Gi"}},
}

// resources returns requests == limits (Guaranteed QoS), so memory_limiter
// can rely on the limit and the scheduler reserves what the collector uses.
func resources(def size, override *monitoringv1alpha1.ResourceSize) corev1.ResourceRequirements {
	cpu := resource.MustParse(def.cpu)
	mem := resource.MustParse(def.memory)
	if override != nil {
		if override.CPU != nil {
			cpu = *override.CPU
		}
		if override.Memory != nil {
			mem = *override.Memory
		}
	}
	rl := corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}
}

func agentResources(cm *monitoringv1alpha1.ClusterMonitoring) corev1.ResourceRequirements {
	var o *monitoringv1alpha1.ResourceSize
	if cm.Spec.CollectorResources != nil {
		o = cm.Spec.CollectorResources.PerNode
	}
	return resources(defaultSizes[cm.Spec.Platform].perNode, o)
}

func gatewayResources(cm *monitoringv1alpha1.ClusterMonitoring) corev1.ResourceRequirements {
	var o *monitoringv1alpha1.ResourceSize
	if cm.Spec.CollectorResources != nil {
		o = cm.Spec.CollectorResources.Central
	}
	return resources(defaultSizes[cm.Spec.Platform].central, o)
}

// ---- pod template pieces ----------------------------------------------------

func fieldEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: path},
	}}
}

func collectorContainer(image string, res corev1.ResourceRequirements) corev1.Container {
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(healthPort)},
	}}
	return corev1.Container{
		Name:  "otel-collector",
		Image: image,
		Args:  []string{"--config=/conf/" + configFileName},
		Env: []corev1.EnvVar{
			fieldEnv("K8S_NODE_NAME", "spec.nodeName"),
			fieldEnv("K8S_NODE_IP", "status.hostIP"),
			fieldEnv("K8S_POD_NAME", "metadata.name"),
			fieldEnv("K8S_POD_NAMESPACE", "metadata.namespace"),
		},
		Resources:      res,
		LivenessProbe:  probe,
		ReadinessProbe: probe,
		VolumeMounts:   []corev1.VolumeMount{{Name: "config", MountPath: "/conf", ReadOnly: true}},
	}
}

func configVolume(configMapName string) corev1.Volume {
	return corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName}},
	}}
}

// podSecurity runs as the collector image's non-root user. On OpenShift the
// UID is left to the SCC, which assigns one from the namespace range.
func podSecurity(cm *monitoringv1alpha1.ClusterMonitoring) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if cm.Spec.Platform != monitoringv1alpha1.PlatformOCP {
		sc.RunAsUser = ptr.To[int64](10001)
	}
	return sc
}

func restrictedContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// ---- ConfigMaps -------------------------------------------------------------

func mutateConfigMap(cm *monitoringv1alpha1.ClusterMonitoring, obj *corev1.ConfigMap, component, config string) {
	obj.Labels = componentLabels(cm, component)
	obj.Data = map[string]string{configFileName: config}
}

// ---- agent DaemonSet ----------------------------------------------------------

func mutateAgentDaemonSet(cm *monitoringv1alpha1.ClusterMonitoring, ds *appsv1.DaemonSet, image, config string) {
	labels := componentLabels(cm, componentAgent)
	ds.Labels = labels

	container := collectorContainer(image, agentResources(cm))
	container.SecurityContext = restrictedContainerSecurity()
	volumes := []corev1.Volume{configVolume(agentName(cm))}
	podSec := podSecurity(cm)

	if cm.Spec.Collect.LogsEnabled() {
		// Container log files on the host are root-owned (0640 on most
		// distros), so log collection needs root and a hostPath mount.
		volumes = append(volumes, corev1.Volume{Name: "varlogpods", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/var/log/pods"},
		}})
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: "varlogpods", MountPath: "/var/log/pods", ReadOnly: true})
		podSec.RunAsNonRoot = ptr.To(false)
		podSec.RunAsUser = ptr.To[int64](0)
		if cm.Spec.Platform == monitoringv1alpha1.PlatformOCP {
			// SELinux on OpenShift blocks hostPath reads unless privileged;
			// the controller binds the SA to the privileged SCC for this.
			container.SecurityContext.Privileged = ptr.To(true)
			container.SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
		}
	}

	ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(cm, componentAgent)}
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
		Type: appsv1.RollingUpdateDaemonSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDaemonSet{
			MaxUnavailable: ptr.To(intstr.FromString("25%")),
		},
	}
	ds.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
			// Changing the config rolls the pods.
			Annotations: map[string]string{configHashAnnotation: hash(config)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccountName(cm),
			SecurityContext:    podSec,
			Containers:         []corev1.Container{container},
			Volumes:            volumes,
			// Run on every node, including tainted control-plane / GPU nodes.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		},
	}
}

// ---- gateway Deployment + Service ---------------------------------------------

func mutateGatewayDeployment(cm *monitoringv1alpha1.ClusterMonitoring, dep *appsv1.Deployment, image, config string) {
	labels := componentLabels(cm, componentGateway)
	dep.Labels = labels

	container := collectorContainer(image, gatewayResources(cm))
	container.SecurityContext = restrictedContainerSecurity()
	container.Ports = []corev1.ContainerPort{
		{Name: "otlp-grpc", ContainerPort: otlpGRPCPort, Protocol: corev1.ProtocolTCP},
		{Name: "otlp-http", ContainerPort: otlpHTTPPort, Protocol: corev1.ProtocolTCP},
	}

	// Exactly one replica: k8s_cluster and k8sobjects would emit duplicates
	// if more than one gateway ran them.
	dep.Spec.Replicas = ptr.To[int32](1)
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(cm, componentGateway)}
	dep.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: map[string]string{configHashAnnotation: hash(config)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccountName(cm),
			SecurityContext:    podSecurity(cm),
			Containers:         []corev1.Container{container},
			Volumes:            []corev1.Volume{configVolume(gatewayName(cm))},
		},
	}
}

func mutateGatewayService(cm *monitoringv1alpha1.ClusterMonitoring, svc *corev1.Service) {
	svc.Labels = componentLabels(cm, componentGateway)
	svc.Spec.Selector = selectorLabels(cm, componentGateway)
	svc.Spec.Ports = []corev1.ServicePort{
		{Name: "otlp-grpc", Port: otlpGRPCPort, TargetPort: intstr.FromString("otlp-grpc"), Protocol: corev1.ProtocolTCP},
		{Name: "otlp-http", Port: otlpHTTPPort, TargetPort: intstr.FromString("otlp-http"), Protocol: corev1.ProtocolTCP},
	}
}
