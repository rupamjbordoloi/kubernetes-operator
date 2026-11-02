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
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1alpha1 "github.com/hdfc/drishti-operator/api/v1alpha1"
)

// duplicateRequeue is how often a non-active (duplicate) ClusterMonitoring
// re-checks whether it has become the active one.
const duplicateRequeue = time.Minute

// ClusterMonitoringReconciler reconciles a ClusterMonitoring object
type ClusterMonitoringReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Options Options
}

// summary is what one reconcile produced; it drives the Ready condition.
type summary struct {
	agentEnabled bool
	agentReady   int32
	agentDesired int32
	agentSettled bool
	gatewayReady bool
}

// The operator's own permissions. It must hold every permission it grants to
// the collectors (Kubernetes RBAC escalation prevention), so the read rules
// below mirror collectorRules().
//
// +kubebuilder:rbac:groups=monitoring.drishti.io,resources=clustermonitorings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.drishti.io,resources=clustermonitorings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.drishti.io,resources=clustermonitorings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes;nodes/stats;nodes/proxy;pods;events;endpoints;replicationcontrollers;resourcequotas;persistentvolumes;persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quota.openshift.io,resources=clusterresourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

// Reconcile turns one ClusterMonitoring into the collectors and RBAC that
// monitor the cluster, then reports their health in status.conditions.
func (r *ClusterMonitoringReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	opts := r.Options.withDefaults()

	cm := &monitoringv1alpha1.ClusterMonitoring{}
	if err := r.Get(ctx, req.NamespacedName, cm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Children carry owner references, so garbage collection removes them.
	if !cm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	active, err := r.activeName(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	// active is empty only if the cache has not seen cm yet; treat cm as active.
	if active != "" && active != cm.Name {
		msg := fmt.Sprintf("only one ClusterMonitoring per cluster is allowed; %q is active", active)
		return ctrl.Result{RequeueAfter: duplicateRequeue},
			r.setReady(ctx, cm, metav1.ConditionFalse, monitoringv1alpha1.ReasonDuplicate, msg)
	}

	sum, err := r.reconcileComponents(ctx, cm, opts)
	if err != nil {
		log.Error(err, "Failed to reconcile components")
		if statusErr := r.setReady(ctx, cm, metav1.ConditionFalse,
			monitoringv1alpha1.ReasonReconcileError, err.Error()); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	status, reason, msg := sum.readyCondition()
	return ctrl.Result{}, r.setReady(ctx, cm, status, reason, msg)
}

// activeName returns the oldest ClusterMonitoring; it is the only one acted on.
func (r *ClusterMonitoringReconciler) activeName(ctx context.Context) (string, error) {
	list := &monitoringv1alpha1.ClusterMonitoringList{}
	if err := r.List(ctx, list); err != nil {
		return "", err
	}
	items := make([]monitoringv1alpha1.ClusterMonitoring, 0, len(list.Items))
	for _, it := range list.Items {
		if it.DeletionTimestamp.IsZero() {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return "", nil
	}
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return items[i].Name < items[j].Name
	})
	return items[0].Name, nil
}

func (r *ClusterMonitoringReconciler) reconcileComponents(ctx context.Context,
	cm *monitoringv1alpha1.ClusterMonitoring, opts Options) (summary, error) {
	var sum summary
	ns := opts.Namespace
	env := opts.environment(cm)

	if err := r.ensureNamespace(ctx, ns); err != nil {
		return sum, err
	}

	// 1. Identity + RBAC shared by both collectors.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName(cm), Namespace: ns}}
	if err := r.apply(ctx, cm, sa, func() { sa.Labels = commonLabels(cm) }); err != nil {
		return sum, err
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(cm)}}
	if err := r.apply(ctx, cm, role, func() { mutateClusterRole(cm, role) }); err != nil {
		return sum, err
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(cm)}}
	if err := r.apply(ctx, cm, binding, func() {
		mutateClusterRoleBinding(cm, binding, clusterRoleName(cm), ns)
	}); err != nil {
		return sum, err
	}

	// OpenShift only, and only when host log files are read.
	scc := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: sccBindingName(cm)}}
	if cm.Spec.Platform == monitoringv1alpha1.PlatformOCP && cm.Spec.Collect.LogsEnabled() {
		if err := r.apply(ctx, cm, scc, func() {
			mutateClusterRoleBinding(cm, scc, openShiftPrivilegedSCCRole, ns)
		}); err != nil {
			return sum, err
		}
	} else if err := r.deleteIfExists(ctx, scc); err != nil {
		return sum, err
	}

	// 2. Gateway: the single path to the backend.
	gatewayCfg, err := renderGatewayConfig(cm, env, opts.destination(cm))
	if err != nil {
		return sum, err
	}
	gwCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: gatewayName(cm), Namespace: ns}}
	if err := r.apply(ctx, cm, gwCM, func() { mutateConfigMap(cm, gwCM, componentGateway, gatewayCfg) }); err != nil {
		return sum, err
	}
	gwSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gatewayName(cm), Namespace: ns}}
	if err := r.apply(ctx, cm, gwSvc, func() { mutateGatewayService(cm, gwSvc) }); err != nil {
		return sum, err
	}
	gwDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gatewayName(cm), Namespace: ns}}
	if err := r.apply(ctx, cm, gwDep, func() {
		mutateGatewayDeployment(cm, gwDep, opts.CollectorImage, gatewayCfg)
	}); err != nil {
		return sum, err
	}
	sum.gatewayReady = deploymentReady(gwDep)

	// 3. Agent: one per node, only if something node-local is collected.
	agentCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentName(cm), Namespace: ns}}
	agentDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: agentName(cm), Namespace: ns}}
	sum.agentEnabled = agentEnabled(cm)
	if sum.agentEnabled {
		agentCfg, err := renderAgentConfig(cm, gatewayEndpoint(cm, ns), ns)
		if err != nil {
			return sum, err
		}
		if err := r.apply(ctx, cm, agentCM, func() { mutateConfigMap(cm, agentCM, componentAgent, agentCfg) }); err != nil {
			return sum, err
		}
		if err := r.apply(ctx, cm, agentDS, func() {
			mutateAgentDaemonSet(cm, agentDS, opts.CollectorImage, agentCfg)
		}); err != nil {
			return sum, err
		}
		sum.agentDesired = agentDS.Status.DesiredNumberScheduled
		sum.agentReady = agentDS.Status.NumberReady
		sum.agentSettled = daemonSetReady(agentDS)
	} else {
		if err := r.deleteIfExists(ctx, agentDS); err != nil {
			return sum, err
		}
		if err := r.deleteIfExists(ctx, agentCM); err != nil {
			return sum, err
		}
	}

	return sum, nil
}

// apply creates or updates obj, owned by cm. Owner references from namespaced
// children to the cluster-scoped ClusterMonitoring are valid, so deleting the
// CR removes everything.
func (r *ClusterMonitoringReconciler) apply(ctx context.Context, cm *monitoringv1alpha1.ClusterMonitoring,
	obj client.Object, mutate func()) error {
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		mutate()
		return controllerutil.SetControllerReference(cm, obj, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("Applied", "kind", fmt.Sprintf("%T", obj),
			"name", obj.GetName(), "namespace", obj.GetNamespace(), "operation", op)
	}
	return nil
}

func (r *ClusterMonitoringReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// ensureNamespace creates the namespace if missing. It is never deleted by
// the operator: it may hold other things.
func (r *ClusterMonitoringReconciler) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: name}, ns)
	if !apierrors.IsNotFound(err) {
		return err
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			ManagedByLabel: ManagedByValue,
			// The agent mounts /var/log/pods (hostPath) when logs are on.
			"pod-security.kubernetes.io/enforce": "privileged",
		},
	}}
	if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *ClusterMonitoringReconciler) setReady(ctx context.Context, cm *monitoringv1alpha1.ClusterMonitoring,
	status metav1.ConditionStatus, reason, msg string) error {
	before := cm.DeepCopy()
	cm.Status.ObservedGeneration = cm.Generation
	meta.SetStatusCondition(&cm.Status.Conditions, metav1.Condition{
		Type:               monitoringv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cm.Generation,
	})
	// Skip no-op writes so status updates don't cause a reconcile loop.
	if equality.Semantic.DeepEqual(before.Status, cm.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, cm, client.MergeFrom(before)))
}

func deploymentReady(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want
}

func daemonSetReady(ds *appsv1.DaemonSet) bool {
	s := ds.Status
	return s.ObservedGeneration >= ds.Generation && s.DesiredNumberScheduled > 0 &&
		s.UpdatedNumberScheduled == s.DesiredNumberScheduled && s.NumberReady == s.DesiredNumberScheduled
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// readyCondition renders the summary, e.g. "2 collectors created".
func (s summary) readyCondition() (metav1.ConditionStatus, string, string) {
	collectors := 1
	if s.agentEnabled {
		collectors++
	}
	msg := plural(collectors, "collector") + " created"

	if !s.gatewayReady || (s.agentEnabled && !s.agentSettled) {
		progress := "gateway not ready"
		if s.gatewayReady {
			progress = "gateway ready"
		}
		if s.agentEnabled {
			progress += fmt.Sprintf(", agent %d/%d nodes ready", s.agentReady, s.agentDesired)
		}
		return metav1.ConditionFalse, monitoringv1alpha1.ReasonProgressing, progress + "; " + msg
	}
	return metav1.ConditionTrue, monitoringv1alpha1.ReasonAllComponentsRunning, msg
}

// SetupWithManager sets up the controller with the Manager. Status changes
// of owned workloads re-trigger reconcile, which keeps the Ready condition live.
func (r *ClusterMonitoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1alpha1.ClusterMonitoring{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Named("clustermonitoring").
		Complete(r)
}
