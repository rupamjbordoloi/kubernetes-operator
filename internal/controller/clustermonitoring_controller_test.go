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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

var _ = Describe("ClusterMonitoring Controller", func() {
	const (
		resourceName = "hlr-gke-prod"
		ns           = DefaultNamespace
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName}

	newReconciler := func() *ClusterMonitoringReconciler {
		return &ClusterMonitoringReconciler{
			Client:  k8sClient,
			Scheme:  k8sClient.Scheme(),
			Options: Options{Destination: monitoringv1alpha1.DestinationSpec{Metrics: "http://mimir:9009/otlp"}},
		}
	}

	reconcileOnce := func(name string) {
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	createCR := func(name string, collect monitoringv1alpha1.CollectSpec) {
		cm := &monitoringv1alpha1.ClusterMonitoring{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.ClusterMonitoringSpec{
				Platform:    monitoringv1alpha1.PlatformGKE,
				ClusterName: "hlr-gke-prod",
				Environment: "prod",
				Collect:     collect,
			},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
	}

	deleteCR := func(name string) {
		cm := &monitoringv1alpha1.ClusterMonitoring{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, cm)
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
	}

	AfterEach(func() {
		deleteCR(resourceName)
		deleteCR("second")
		// envtest has no garbage collector, so remove children by hand.
		for _, obj := range []client.Object{
			&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-agent", Namespace: ns}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-gateway", Namespace: ns}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-agent", Namespace: ns}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-gateway", Namespace: ns}},
		} {
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, obj))
		}
		// Ensure the CRs are really gone before the next spec.
		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, key, &monitoringv1alpha1.ClusterMonitoring{}))
		}, 5*time.Second).Should(BeTrue())
	})

	It("applies API defaults from the CRD schema", func() {
		createCR(resourceName, monitoringv1alpha1.CollectSpec{})
		cm := &monitoringv1alpha1.ClusterMonitoring{}
		Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
		Expect(cm.Spec.Collect.Nodes).To(Equal(ptr.To(true)))
		Expect(cm.Spec.Collect.Logs).To(Equal(ptr.To(false)))
		Expect(cm.Spec.CollectionInterval.Duration).To(Equal(30 * time.Second))
		Expect(cm.Spec.DefaultAlerts).To(Equal(ptr.To(true)))
	})

	It("creates collectors, RBAC and dashboard, and reports progress", func() {
		createCR(resourceName, monitoringv1alpha1.CollectSpec{Logs: ptr.To(true)})
		reconcileOnce(resourceName)

		By("creating the per-node agent with the log volume")
		ds := &appsv1.DaemonSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-agent", Namespace: ns}, ds)).To(Succeed())
		Expect(ds.Spec.Template.Spec.ServiceAccountName).To(Equal(resourceName + "-collector"))
		Expect(ds.Spec.Template.Spec.Volumes).To(ContainElement(HaveField("Name", "varlogpods")))
		Expect(ds.Spec.Template.Annotations).To(HaveKey(configHashAnnotation))
		Expect(ds.OwnerReferences).To(HaveLen(1))

		By("creating the gateway with the backend endpoint in its config")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-gateway", Namespace: ns},
			&appsv1.Deployment{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-gateway", Namespace: ns},
			&corev1.Service{})).To(Succeed())
		gwCM := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-gateway", Namespace: ns}, gwCM)).To(Succeed())
		Expect(gwCM.Data[configFileName]).To(ContainSubstring("http://mimir:9009/otlp"))
		Expect(gwCM.Data[configFileName]).To(ContainSubstring("k8sobjects"))

		By("creating cluster-wide RBAC for the collectors")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "drishti:" + resourceName + ":collector"},
			&rbacv1.ClusterRole{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "drishti:" + resourceName + ":collector"},
			&rbacv1.ClusterRoleBinding{})).To(Succeed())

		By("creating the Grafana dashboard ConfigMap")
		dash := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-platform-dashboard", Namespace: ns},
			dash)).To(Succeed())
		Expect(dash.Labels).To(HaveKeyWithValue(grafanaDashboardLabel, "1"))

		By("reporting Progressing, since envtest runs no pods, and skipping alerts without the CRD")
		cm := &monitoringv1alpha1.ClusterMonitoring{}
		Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
		cond := meta.FindStatusCondition(cm.Status.Conditions, monitoringv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(monitoringv1alpha1.ReasonProgressing))
		Expect(cond.Message).To(ContainSubstring("2 collectors, 1 dashboard, 0 alert rules created"))
		Expect(cond.Message).To(ContainSubstring("PrometheusRule CRD not installed"))
	})

	It("removes the agent when nothing node-local is collected", func() {
		createCR(resourceName, monitoringv1alpha1.CollectSpec{})
		reconcileOnce(resourceName)
		agentKey := types.NamespacedName{Name: resourceName + "-agent", Namespace: ns}
		Expect(k8sClient.Get(ctx, agentKey, &appsv1.DaemonSet{})).To(Succeed())

		cm := &monitoringv1alpha1.ClusterMonitoring{}
		Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
		cm.Spec.Collect = monitoringv1alpha1.CollectSpec{
			Nodes: ptr.To(false), Pods: ptr.To(false), Logs: ptr.To(false), Events: ptr.To(true),
		}
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())
		reconcileOnce(resourceName)

		err := k8sClient.Get(ctx, agentKey, &appsv1.DaemonSet{})
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("marks a second ClusterMonitoring as duplicate", func() {
		createCR(resourceName, monitoringv1alpha1.CollectSpec{})
		time.Sleep(1100 * time.Millisecond) // creationTimestamp has 1s resolution
		createCR("second", monitoringv1alpha1.CollectSpec{})
		reconcileOnce("second")

		second := &monitoringv1alpha1.ClusterMonitoring{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "second"}, second)).To(Succeed())
		cond := meta.FindStatusCondition(second.Status.Conditions, monitoringv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(monitoringv1alpha1.ReasonDuplicate))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "second-gateway", Namespace: ns},
			&appsv1.Deployment{})).NotTo(Succeed())
	})
})
