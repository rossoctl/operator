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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentv1alpha1 "github.com/rossoctl/operator/api/v1alpha1"
	"github.com/rossoctl/operator/internal/agentcard"
	"github.com/rossoctl/operator/internal/signature"
	webhookconfig "github.com/rossoctl/operator/internal/webhook/config"
	"github.com/rossoctl/operator/internal/workload"
)

const (
	AgentRuntimeFinalizer = "rossoctl.io/cleanup"

	// AnnotationConfigHash is the annotation applied to PodTemplateSpec to trigger rolling updates.
	AnnotationConfigHash = "rossoctl.io/config-hash"

	// AnnotationSkills is read from target workloads to discover linked skills.
	// Value is a JSON array of skill names, set by the rossoctl backend or the user.
	AnnotationSkills = "rossoctl.io/skills"

	// AnnotationMTLSMode is the annotation applied to PodTemplateSpec to advertise the
	// resolved mTLS posture. Read by authbridge sidecars for observability.
	AnnotationMTLSMode = "rossoctl.io/mtls-mode"

	// AnnotationRestartPending marks a Sandbox that was scaled to 0 and needs
	// to be scaled back to 1 on the next reconcile cycle. Two-phase restart
	// avoids a race with the Sandbox controller's pod-name annotation.
	AnnotationRestartPending = "rossoctl.io/restart-pending"

	// Condition types for AgentRuntime status.
	ConditionTypeReady          = "Ready"
	ConditionTypeTargetResolved = "TargetResolved"
	ConditionTypeConfigResolved = "ConfigResolved"
	ConditionTypeMTLSReady      = "MTLSReady"

	// Condition reasons for AgentRuntime target resolution.
	ReasonTargetFound        = "TargetFound"
	ReasonTargetNotFound     = "TargetNotFound"
	ReasonTargetResolveError = "TargetResolveError"

	// AnnotationLastCardFetchHash stores the change-detection key used to skip
	// redundant card fetches when the workload's pod template has not changed.
	AnnotationLastCardFetchHash = "agent.rossoctl.dev/last-card-fetch-hash"

	// KindSandbox is the workload kind for agent-sandbox CRs.
	KindSandbox = "Sandbox"

	// AnnotationRestartPendingValue is the value set on AnnotationRestartPending.
	AnnotationRestartPendingValue = "true"

	// SPIRE volume names injected by the webhook when mTLS is enabled.
	VolumeSpireAgentSocket = "spire-agent-socket"
	VolumeSVIDOutput       = "svid-output"
)

var sandboxGVK = schema.GroupVersionKind{
	Group:   "agents.x-k8s.io",
	Version: "v1alpha1",
	Kind:    KindSandbox,
}

// errTargetNotFound is a sentinel wrapped by resolveTargetRef when the target
// workload referenced by spec.targetRef does not exist. Reconcile treats this
// as a recoverable degraded state rather than an error, to avoid log/event
// spam while the target is absent.
var errTargetNotFound = errors.New("target workload not found")

// AgentRuntimeReconciler reconciles AgentRuntime objects.
type AgentRuntimeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	APIReader client.Reader // uncached reader for cross-namespace ConfigMap reads

	AgentFetcher         agentcard.Fetcher
	AuthenticatedFetcher agentcard.AuthenticatedFetcher
	SignatureProvider    signature.Provider
	EnableCardDiscovery  bool
	SpireTrustDomain     string
	GetFeatureGates      func() *webhookconfig.FeatureGates
	GetPlatformConfig    func() *webhookconfig.PlatformConfig
}

func (r *AgentRuntimeReconciler) getFeatureGates() *webhookconfig.FeatureGates {
	if r.GetFeatureGates != nil {
		return r.GetFeatureGates()
	}
	return webhookconfig.DefaultFeatureGates()
}

func (r *AgentRuntimeReconciler) getPlatformConfig() *webhookconfig.PlatformConfig {
	if r.GetPlatformConfig != nil {
		if cfg := r.GetPlatformConfig(); cfg != nil {
			return cfg
		}
	}
	return webhookconfig.CompiledDefaults()
}

// +kubebuilder:rbac:groups=agent.rossoctl.dev,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agent.rossoctl.dev,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agent.rossoctl.dev,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get

func (r *AgentRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling AgentRuntime", "namespacedName", req.NamespacedName)

	// 1. Fetch the AgentRuntime CR
	rt := &agentv1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rt.Status.ObservedGeneration = rt.Generation

	// 2. Handle deletion
	if !rt.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, rt)
	}

	// 3. Ensure finalizer
	if !controllerutil.ContainsFinalizer(rt, AgentRuntimeFinalizer) {
		controllerutil.AddFinalizer(rt, AgentRuntimeFinalizer)
		if err := r.Update(ctx, rt); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 4. Resolve targetRef (existence check)
	if err := r.resolveTargetRef(ctx, rt); err != nil {
		if errors.Is(err, errTargetNotFound) {
			// Recoverable: the AgentRuntime outlives its target workload (e.g. the
			// child Sandbox/Deployment was deleted directly). Treat as a degraded
			// state instead of spamming Error logs / Warning events every cycle.
			// Emit the Warning event only on transition into the degraded state,
			// deduped via the pre-existing TargetResolved=False/TargetNotFound
			// condition. Recovery is automatic once the target reappears.
			prev := meta.FindStatusCondition(rt.Status.Conditions, ConditionTypeTargetResolved)
			alreadyDegraded := prev != nil &&
				prev.Status == metav1.ConditionFalse &&
				prev.Reason == ReasonTargetNotFound

			logger.V(1).Info("Target workload not found; AgentRuntime degraded", "error", err.Error())
			r.setDegradedTargetNotFound(ctx, req.NamespacedName, err.Error())
			if r.Recorder != nil && !alreadyDegraded {
				r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, ReasonTargetNotFound,
					"ResolveTarget", err.Error())
			}
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		// Genuine API error resolving the target: keep the loud error path.
		logger.Error(err, "Failed to resolve targetRef")
		r.updateErrorStatus(ctx, req.NamespacedName, ConditionTypeTargetResolved, ReasonTargetResolveError, err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, ReasonTargetResolveError,
				"ResolveTarget", err.Error())
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.setCondition(rt, ConditionTypeTargetResolved, metav1.ConditionTrue, ReasonTargetFound,
		fmt.Sprintf("%s %s resolved", rt.Spec.TargetRef.Kind, rt.Spec.TargetRef.Name))

	// 4.1. Complete two-phase Sandbox restart if pending.
	if rt.Spec.TargetRef.Kind == KindSandbox {
		if result, done, err := r.completeSandboxRestart(ctx, rt); done {
			return result, err
		}
	}

	// 4.5. Ensure required authbridge ConfigMaps exist in the namespace.
	// Copies templates from rossoctl-system if missing, matching the backend's
	// _ensure_authbridge_configmaps() semantics (create-if-not-exists).
	if err := r.ensureNamespaceConfigMaps(ctx, rt.Namespace); err != nil {
		logger.Error(err, "Failed to ensure namespace ConfigMaps")
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "ConfigMapEnsureError",
				"EnsureConfigMaps", err.Error())
		}
	}

	// 4.5b. Ensure spiffe-helper-config CM is derived from PlatformConfig.
	// Unlike template CMs above, this always overwrites to keep PlatformConfig
	// as the single source of truth.
	if err := r.ensureSpiffeHelperConfigMap(ctx, rt.Namespace); err != nil {
		logger.Error(err, "Failed to ensure spiffe-helper-config")
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "ConfigMapEnsureError",
				"EnsureSpiffeHelperConfig", err.Error())
		}
		r.updateErrorStatus(ctx, req.NamespacedName, ConditionTypeReady, "SpiffeHelperConfigError", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 4.6. Ensure namespace has Istio ambient mesh labels for ztunnel mTLS.
	istioLabeled, istioErr := r.ensureIstioMeshLabels(ctx, rt.Namespace)
	switch {
	case istioErr != nil:
		logger.Error(istioErr, "Failed to ensure Istio mesh labels")
		r.setCondition(rt, ConditionTypeIstioMeshEnrolled, metav1.ConditionFalse, "PatchFailed", istioErr.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "IstioMeshLabelError",
				"EnsureIstioMesh", istioErr.Error())
		}
	case istioLabeled:
		r.setCondition(rt, ConditionTypeIstioMeshEnrolled, metav1.ConditionTrue, "NamespaceLabeled",
			fmt.Sprintf("Namespace %s enrolled in Istio ambient mesh", rt.Namespace))
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeNormal, "IstioMeshEnrolled",
				"EnsureIstioMesh", "Namespace %s labeled for Istio ambient mesh", rt.Namespace)
		}
	default:
		r.setCondition(rt, ConditionTypeIstioMeshEnrolled, metav1.ConditionFalse, "OptedOut",
			fmt.Sprintf("Namespace %s opted out of Istio mesh enrollment", rt.Namespace))
	}

	// 4.7. Evaluate MTLSReady condition based on resolved mTLSMode and SPIRE availability.
	r.evaluateMTLSReady(ctx, rt)

	// 4.8. Ensure SCC RoleBinding exists in the namespace.
	// Creates a RoleBinding granting all ServiceAccounts in the namespace
	// access to the rossoctl-authbridge SCC. No-op on non-OpenShift clusters.
	// On OpenShift, a transient failure is retried via requeue to prevent
	// agent pods from failing with SCC violations at runtime.
	if err := r.ensureNamespaceSCCBinding(ctx, rt.Namespace); err != nil {
		logger.Error(err, "Failed to ensure SCC RoleBinding")
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "SCCBindingError",
				"EnsureSCCBinding", err.Error())
		}
		r.updateErrorStatus(ctx, req.NamespacedName, ConditionTypeReady, "SCCBindingError", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 5. Compute config hash from merged configuration (cluster → namespace)
	configResult, err := ComputeConfigHash(ctx, r.uncachedReader(), rt.Namespace)
	if err != nil {
		logger.Error(err, "Failed to compute config hash")
		r.updateErrorStatus(ctx, req.NamespacedName, ConditionTypeReady, "ConfigHashError", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Surface config resolution warnings (e.g., multiple namespace defaults ConfigMaps)
	if len(configResult.Warnings) > 0 {
		r.setCondition(rt, ConditionTypeConfigResolved, metav1.ConditionTrue, "ConfigWarning",
			strings.Join(configResult.Warnings, "; "))
		if r.Recorder != nil {
			for _, w := range configResult.Warnings {
				r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "ConfigWarning",
					"ResolveConfig", w)
			}
		}
	} else {
		r.setCondition(rt, ConditionTypeConfigResolved, metav1.ConditionTrue, "ConfigResolved",
			"Configuration resolved successfully")
	}

	// 5.5. Card discovery phase: fetch agent card from Service endpoint
	r.fetchAndUpdateCard(ctx, rt)

	// 6. Apply labels and annotations to the target workload
	if err := r.applyWorkloadConfig(ctx, rt, configResult.Hash); err != nil {
		logger.Error(err, "Failed to apply workload config")
		r.updateErrorStatus(ctx, req.NamespacedName, ConditionTypeReady, "ConfigApplyError", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 6.5. Discover linked skills from workload annotation (set by rossoctl backend or user)
	fg := r.getFeatureGates()
	var linkedSkills []string
	if fg.SkillDiscovery {
		linkedSkills = r.readLinkedSkills(ctx, rt)
	}

	// 7. Count configured pods
	configuredPods, err := r.countConfiguredPods(ctx, rt)
	if err != nil {
		logger.V(1).Info("Failed to count configured pods", "error", err)
	}

	// 8. Update status (retry on conflict to preserve all conditions computed above)
	rt.Status.ConfiguredPods = configuredPods
	r.setCondition(rt, ConditionTypeReady, metav1.ConditionTrue, "Configured",
		fmt.Sprintf("Workload %s configured with config-hash %s", rt.Spec.TargetRef.Name, configResult.Hash[:12]))
	if fg.SkillDiscovery {
		rt.Status.LinkedSkills = linkedSkills
	} else {
		rt.Status.LinkedSkills = nil
	}
	desired := rt.Status.DeepCopy()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
			return err
		}
		rt.Status = *desired // safe: this controller is the sole status owner
		return r.Status().Update(ctx, rt)
	}); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(rt, nil, corev1.EventTypeNormal, "Configured",
			"ApplyConfig", "Applied config to %s %s", rt.Spec.TargetRef.Kind, rt.Spec.TargetRef.Name)
	}

	return ctrl.Result{}, nil
}

// resolveTargetRef verifies that the workload referenced by spec.targetRef exists.
func (r *AgentRuntimeReconciler) resolveTargetRef(ctx context.Context, rt *agentv1alpha1.AgentRuntime) error {
	ref := rt.Spec.TargetRef

	if _, err := schema.ParseGroupVersion(ref.APIVersion); err != nil {
		return fmt.Errorf("invalid apiVersion %s: %w", ref.APIVersion, err)
	}

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return fmt.Errorf("unsupported workload kind: %s", ref.Kind)
	}

	key := client.ObjectKey{Namespace: rt.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, acc.obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s/%s %s not found in namespace %s: %w",
				ref.APIVersion, ref.Kind, ref.Name, rt.Namespace, errTargetNotFound)
		}
		return err
	}

	return nil
}

// applyWorkloadConfig applies rossoctl labels and config-hash annotation to the
// target workload's metadata and PodTemplateSpec.
func (r *AgentRuntimeReconciler) applyWorkloadConfig(ctx context.Context, rt *agentv1alpha1.AgentRuntime, configHash string) error {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return fmt.Errorf("unsupported workload kind: %s", ref.Kind)
	}

	key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}

	// Resolve mTLS mode: CR value takes precedence, default to "permissive".
	mtlsMode := rt.Spec.MTLSMode
	if mtlsMode == "" {
		mtlsMode = "permissive"
	}

	var configHashChanged bool

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, key, acc.obj); err != nil {
			return err
		}

		// Check if update is needed before mutating
		currentWorkloadLabels := acc.obj.GetLabels()
		currentPodLabels := acc.getPodLabels(acc.obj)
		currentPodAnnotations := acc.getPodAnnotations(acc.obj)

		alreadyConfigured := currentWorkloadLabels[LabelAgentType] == string(rt.Spec.Type) &&
			currentWorkloadLabels[LabelManagedBy] == LabelManagedByValue &&
			currentPodLabels[LabelAgentType] == string(rt.Spec.Type) &&
			currentPodAnnotations[AnnotationConfigHash] == configHash &&
			currentPodAnnotations[AnnotationMTLSMode] == mtlsMode

		if alreadyConfigured {
			return nil
		}

		// Track whether config-hash actually changed (for Sandbox rollout)
		previousHash := currentPodAnnotations[AnnotationConfigHash]
		configHashChanged = previousHash != "" && previousHash != configHash

		// Apply labels to workload metadata
		workloadLabels := acc.obj.GetLabels()
		if workloadLabels == nil {
			workloadLabels = make(map[string]string)
		}
		workloadLabels[LabelAgentType] = string(rt.Spec.Type)
		workloadLabels[LabelManagedBy] = LabelManagedByValue
		acc.obj.SetLabels(workloadLabels)

		// Apply labels to PodTemplateSpec
		podLabels := acc.getPodLabels(acc.obj)
		if podLabels == nil {
			podLabels = make(map[string]string)
		}
		podLabels[LabelAgentType] = string(rt.Spec.Type)
		acc.setPodLabels(acc.obj, podLabels)

		// Apply config-hash and mtls-mode annotations to PodTemplateSpec
		podAnnotations := acc.getPodAnnotations(acc.obj)
		if podAnnotations == nil {
			podAnnotations = make(map[string]string)
		}
		podAnnotations[AnnotationConfigHash] = configHash
		podAnnotations[AnnotationMTLSMode] = mtlsMode
		acc.setPodAnnotations(acc.obj, podAnnotations)

		logger.Info("Applying config to workload",
			"workload", ref.Name,
			"kind", ref.Kind,
			"type", string(rt.Spec.Type),
			"configHash", configHash[:12],
			"mtlsMode", mtlsMode)

		return r.Update(ctx, acc.obj)
	})
	if err != nil {
		return err
	}

	// Sandbox pods don't restart on podTemplate changes (upstream limitation).
	// Phase 1: scale to 0 and mark restart-pending. Phase 2 runs on the next
	// reconcile (triggered by the Sandbox watch) to clear stale annotations
	// and scale back to 1. Two-phase avoids a race with the Sandbox controller.
	if ref.Kind == KindSandbox && configHashChanged {
		if err := r.beginSandboxRestart(ctx, key); err != nil {
			return fmt.Errorf("sandbox restart (phase 1) failed: %w", err)
		}
	}

	return nil
}

// beginSandboxRestart is phase 1 of a two-phase Sandbox restart.
// It scales the Sandbox to 0 replicas and sets the restart-pending annotation.
// Phase 2 (completeSandboxRestart) runs on the next reconcile to clear the
// stale pod-name annotation and scale back to 1.
func (r *AgentRuntimeReconciler) beginSandboxRestart(ctx context.Context, key types.NamespacedName) error {
	logger := log.FromContext(ctx)
	logger.Info("Sandbox restart phase 1: scaling to 0", "sandbox", key.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(sandboxGVK)
		if err := r.Get(ctx, key, obj); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", "replicas"); err != nil {
			return err
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationRestartPending] = AnnotationRestartPendingValue
		obj.SetAnnotations(annotations)
		return r.Update(ctx, obj)
	})
}

// completeSandboxRestart is phase 2 of a two-phase Sandbox restart.
// It checks for the restart-pending annotation on a Sandbox with replicas=0,
// clears the stale pod-name annotation, removes restart-pending, and scales
// back to 1. Returns (result, true, err) if it handled the restart, or
// (_, false, nil) if no restart was pending.
func (r *AgentRuntimeReconciler) completeSandboxRestart(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef
	key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(sandboxGVK)
	if err := r.Get(ctx, key, obj); err != nil {
		return ctrl.Result{}, false, nil
	}

	annotations := obj.GetAnnotations()
	if annotations[AnnotationRestartPending] != AnnotationRestartPendingValue {
		return ctrl.Result{}, false, nil
	}

	logger.Info("Sandbox restart phase 2: clearing pod-name and scaling to 1", "sandbox", key.Name)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(sandboxGVK)
		if err := r.Get(ctx, key, obj); err != nil {
			return err
		}
		annotations := obj.GetAnnotations()
		delete(annotations, "agents.x-k8s.io/pod-name")
		delete(annotations, AnnotationRestartPending)
		obj.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(obj.Object, int64(1), "spec", "replicas"); err != nil {
			return err
		}
		return r.Update(ctx, obj)
	})
	if err != nil {
		logger.Error(err, "Sandbox restart phase 2 failed, will retry")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(rt, nil, corev1.EventTypeNormal, "SandboxRestarted",
			"RestartSandbox", "Sandbox %s restarted via scale 0→1", ref.Name)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
}

// countConfiguredPods counts pods that have the rossoctl.io/type label matching the runtime type.
func (r *AgentRuntimeReconciler) countConfiguredPods(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (int32, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(rt.Namespace),
		client.MatchingLabels{LabelAgentType: string(rt.Spec.Type)},
	); err != nil {
		return 0, err
	}

	var count int32
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isPodOwnedByWorkload(pod, rt.Spec.TargetRef.Name) {
			count++
		}
	}
	return count, nil
}

// readLinkedSkills reads the rossoctl.io/skills annotation from the target
// workload and returns the skill names. This annotation is set by the rossoctl
// backend (PR #1440) or manually by the user — the operator reads but never
// sets it.
func (r *AgentRuntimeReconciler) readLinkedSkills(ctx context.Context, rt *agentv1alpha1.AgentRuntime) []string {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return nil
	}

	key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}
	if err := r.Get(ctx, key, acc.obj); err != nil {
		logger.V(1).Info("Failed to read workload for skill annotation", "error", err)
		return nil
	}

	annotations := acc.obj.GetAnnotations()
	if annotations == nil {
		return nil
	}

	raw, ok := annotations[AnnotationSkills]
	if !ok || raw == "" {
		return nil
	}

	var skills []string
	if err := json.Unmarshal([]byte(raw), &skills); err != nil {
		logger.V(1).Info("Failed to parse rossoctl.io/skills annotation", "error", err, "raw", raw)
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "SkillAnnotationParseError",
				"ReadLinkedSkills", "Failed to parse rossoctl.io/skills annotation: %v", err)
		}
		return nil
	}
	return skills
}

// resolveServiceForWorkload finds the Service that fronts the target workload.
// It first tries a Service with the same name as the Deployment (standard convention),
// then falls back to selector matching against the Deployment's pod template labels.
func (r *AgentRuntimeReconciler) resolveServiceForWorkload(ctx context.Context, namespace string, ref agentv1alpha1.TargetRef) (*corev1.Service, int32, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, svc); err == nil {
		port := serviceHTTPPort(ctx, svc)
		logger.V(1).Info("Resolved service by name", "service", ref.Name, "port", port)
		return svc, port, nil
	}

	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return nil, 0, fmt.Errorf("unsupported workload kind for service resolution: %s", ref.Kind)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, acc.obj); err != nil {
		return nil, 0, fmt.Errorf("failed to get workload %s/%s: %w", ref.Kind, ref.Name, err)
	}
	podLabels := acc.getPodLabels(acc.obj)
	if len(podLabels) == 0 {
		return nil, 0, fmt.Errorf("workload %s/%s has no pod template labels for selector matching", ref.Kind, ref.Name)
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(namespace)); err != nil {
		return nil, 0, fmt.Errorf("failed to list services: %w", err)
	}

	for i := range svcList.Items {
		s := &svcList.Items[i]
		if s.Spec.Selector == nil {
			continue
		}
		if selectorMatchesLabels(s.Spec.Selector, podLabels) {
			port := serviceHTTPPort(ctx, s)
			logger.V(1).Info("Resolved service by selector match", "service", s.Name, "port", port)
			return s, port, nil
		}
	}

	return nil, 0, fmt.Errorf("no Service matches workload %s/%s in namespace %s", ref.Kind, ref.Name, namespace)
}

// checkWorkloadReady checks whether the target workload has at least one ready
// replica. For Sandboxes, the check is skipped (always returns true) because
// their lifecycle is managed by the sandbox controller.
func (r *AgentRuntimeReconciler) checkWorkloadReady(ctx context.Context, namespace string, ref agentv1alpha1.TargetRef) (bool, string) {
	switch ref.Kind {
	case "Deployment": //nolint:goconst
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, dep); err != nil {
			return false, fmt.Sprintf("failed to get Deployment %s: %v", ref.Name, err)
		}
		if dep.Status.ReadyReplicas == 0 {
			return false, fmt.Sprintf("Deployment %s has 0 ready replicas", ref.Name)
		}
	case "StatefulSet": //nolint:goconst
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, sts); err != nil {
			return false, fmt.Sprintf("failed to get StatefulSet %s: %v", ref.Name, err)
		}
		if sts.Status.ReadyReplicas == 0 {
			return false, fmt.Sprintf("StatefulSet %s has 0 ready replicas", ref.Name)
		}
	case KindSandbox:
		// Sandboxes are unstructured; skip readiness check.
	}
	return true, ""
}

func selectorMatchesLabels(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func serviceHTTPPort(ctx context.Context, svc *corev1.Service) int32 {
	logger := log.FromContext(ctx)

	if ann, ok := svc.Annotations["rossoctl.io/port"]; ok {
		port, err := strconv.ParseInt(ann, 10, 32)
		if err == nil && port > 0 {
			return int32(port)
		}
		logger.Info("Invalid rossoctl.io/port annotation, falling back to port name resolution",
			"service", svc.Name, "annotation", ann)
	}

	for _, p := range svc.Spec.Ports {
		if strings.EqualFold(p.Name, "a2a") {
			return p.Port
		}
	}
	for _, p := range svc.Spec.Ports {
		if strings.EqualFold(p.Name, "http") {
			return p.Port
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return svc.Spec.Ports[0].Port
	}
	return 8000
}

func getAgentTLSPort(svc *corev1.Service) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Name == AgentTLSPortName {
			return p.Port
		}
	}
	return 0
}

// isPodOwnedByWorkload checks if a pod is transitively owned by the named workload.
// For Deployments: Pod → ReplicaSet (<deployment>-<pod-template-hash>) → Deployment.
// For StatefulSets: Pod is directly owned by the StatefulSet.
// For Sandboxes: Pod is directly owned by the Sandbox CR.
func isPodOwnedByWorkload(pod *corev1.Pod, workloadName string) bool {
	return workload.IsPodOwnedBy(pod, workloadName)
}

// handleDeletion runs finalizer logic when an AgentRuntime is deleted.
// It removes the rossoctl.io/type label and rossoctl.io/config-hash annotation so that
// the next rolling update creates pods without sidecars, returning the workload to its
// pre-AR state.
func (r *AgentRuntimeReconciler) handleDeletion(ctx context.Context, rt *agentv1alpha1.AgentRuntime) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(rt, AgentRuntimeFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Handling AgentRuntime deletion", "name", rt.Name)

	ref := rt.Spec.TargetRef
	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if ok {
		key := types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}
		updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, acc.obj); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}

			// Remove rossoctl.io/type and rossoctl.io/managed-by from workload metadata.
			workloadLabels := acc.obj.GetLabels()
			delete(workloadLabels, LabelAgentType)
			delete(workloadLabels, LabelManagedBy)
			acc.obj.SetLabels(workloadLabels)

			// Remove rossoctl.io/type from PodTemplateSpec pod labels so future pods
			// are not presented to the webhook with the type label.
			podLabels := acc.getPodLabels(acc.obj)
			delete(podLabels, LabelAgentType)
			acc.setPodLabels(acc.obj, podLabels)

			// Remove rossoctl.io/config-hash and rossoctl.io/mtls-mode from PodTemplateSpec
			// pod annotations. This triggers the rolling update that replaces existing
			// injected pods, and leaves the workload annotation-clean for any future AR.
			podAnnotations := acc.getPodAnnotations(acc.obj)
			delete(podAnnotations, AnnotationConfigHash)
			delete(podAnnotations, AnnotationMTLSMode)
			acc.setPodAnnotations(acc.obj, podAnnotations)

			logger.Info("Removed rossoctl labels and config-hash from workload on AgentRuntime deletion",
				"workload", ref.Name, "kind", ref.Kind)
			return r.Update(ctx, acc.obj)
		})
		if updateErr != nil {
			// Return the error to requeue — don't remove the finalizer until the
			// workload is cleaned up. This prevents the CR from being deleted while
			// the workload retains stale managed-by labels and wrong config-hash.
			logger.Error(updateErr, "Failed to update workload on deletion, will retry")
			return ctrl.Result{}, updateErr
		}
	}

	// Remove finalizer
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentv1alpha1.AgentRuntime{}
		if err := r.Get(ctx, types.NamespacedName{Name: rt.Name, Namespace: rt.Namespace}, latest); err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(latest, AgentRuntimeFinalizer)
		return r.Update(ctx, latest)
	}); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("Removed finalizer from AgentRuntime", "name", rt.Name)
	return ctrl.Result{}, nil
}

// evaluateMTLSReady sets the MTLSReady condition based on the resolved
// mTLSMode and whether SPIRE infrastructure is available on the workload.
// This is informational — MTLSReady=False does NOT block Ready=True.
func (r *AgentRuntimeReconciler) evaluateMTLSReady(ctx context.Context, rt *agentv1alpha1.AgentRuntime) {
	logger := log.FromContext(ctx)

	mtlsMode := rt.Spec.MTLSMode
	if mtlsMode == "" {
		mtlsMode = "permissive"
	}

	if mtlsMode == "disabled" {
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionTrue, "MTLSDisabled",
			"mTLS explicitly disabled on this AgentRuntime")
		return
	}

	// Check if workload has SPIRE infrastructure by looking for
	// spire-agent-socket or svid-output volumes in the pod template.
	acc, ok := newRuntimePodTemplateAccessor(rt.Spec.TargetRef.Kind)
	if !ok {
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionFalse, "UnsupportedKind",
			fmt.Sprintf("Cannot check SPIRE availability for workload kind %s", rt.Spec.TargetRef.Kind))
		return
	}

	key := types.NamespacedName{Name: rt.Spec.TargetRef.Name, Namespace: rt.Namespace}
	if err := r.Get(ctx, key, acc.obj); err != nil {
		logger.V(1).Info("Cannot read workload for SPIRE check", "error", err)
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionFalse, "WorkloadNotFound",
			fmt.Sprintf("Cannot verify SPIRE availability: %v", err))
		return
	}

	podSpec := acc.getPodSpec(acc.obj)
	if podSpec == nil {
		// Sandbox workloads don't expose a typed PodSpec; assume SPIRE
		// is available since the webhook handles injection at pod CREATE.
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionTrue, "SPIREAssumed",
			"Sandbox workload; SPIRE availability assumed (webhook handles injection)")
		return
	}

	spireDetected := false
	for _, v := range podSpec.Volumes {
		if v.Name == VolumeSpireAgentSocket || v.Name == VolumeSVIDOutput {
			spireDetected = true
			break
		}
	}

	if spireDetected {
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionTrue, "SPIREAvailable",
			fmt.Sprintf("SPIRE volumes detected on workload %s; mTLS mode: %s", rt.Spec.TargetRef.Name, mtlsMode))
	} else {
		msg := "mTLS requires SPIRE; either deploy SPIRE or set mTLSMode: disabled"
		r.setCondition(rt, ConditionTypeMTLSReady, metav1.ConditionFalse, "SPIREUnavailable", msg)
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "SPIREUnavailable",
				"MTLSReadyCheck", msg)
		}
		// This check only verifies X.509-SVID volumes for mTLS certificate management.
		// This is expected with Helm-managed SPIRE deployments where volumes aren't
		// pre-injected. Other SPIRE features like SPIFFE auth (JWT-SVID) still work
		// as long as SPIRE is installed and the feature is enabled.
		logger.Info("SPIRE socket volumes not detected (X.509-SVID mTLS unavailable)",
			"workload", rt.Spec.TargetRef.Name, "mtlsMode", mtlsMode)
	}
}

func (r *AgentRuntimeReconciler) setCondition(rt *agentv1alpha1.AgentRuntime, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rt.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: rt.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// updateErrorStatus updates a condition to False with retry-on-conflict
// semantics, re-fetching the object on each attempt.
func (r *AgentRuntimeReconciler) updateErrorStatus(ctx context.Context, key types.NamespacedName, condType, reason, message string) {
	logger := log.FromContext(ctx)
	if statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentv1alpha1.AgentRuntime{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		r.setCondition(latest, condType, metav1.ConditionFalse, reason, message)
		return r.Status().Update(ctx, latest)
	}); statusErr != nil {
		logger.Error(statusErr, "Failed to update error status", "condition", condType, "reason", reason)
	}
}

// setDegradedTargetNotFound marks the AgentRuntime as degraded because its
// target workload is missing: Ready=False and TargetResolved=False, both with
// reason "TargetNotFound". It also clears status.Card, which was discovered
// from the now-absent target and is therefore stale. Other conditions are
// preserved. Recovery is automatic once the target reappears (see
// SetupWithManager workload watches).
func (r *AgentRuntimeReconciler) setDegradedTargetNotFound(ctx context.Context, key types.NamespacedName, message string) {
	logger := log.FromContext(ctx)
	if statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentv1alpha1.AgentRuntime{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		r.setCondition(latest, ConditionTypeTargetResolved, metav1.ConditionFalse, ReasonTargetNotFound, message)
		r.setCondition(latest, ConditionTypeReady, metav1.ConditionFalse, ReasonTargetNotFound, message)
		// The card was discovered from the now-absent target workload; clear it.
		latest.Status.Card = nil
		return r.Status().Update(ctx, latest)
	}); statusErr != nil {
		logger.Error(statusErr, "Failed to update degraded status", "reason", ReasonTargetNotFound)
	}
}

// fetchAndUpdateCard discovers the agent card from the workload's Service endpoint
// and populates status.card. Skips fetch when the feature flag is disabled or
// when the workload's change-detection key has not changed.
func (r *AgentRuntimeReconciler) fetchAndUpdateCard(ctx context.Context, rt *agentv1alpha1.AgentRuntime) {
	logger := log.FromContext(ctx)

	if !r.EnableCardDiscovery {
		if rt.Status.Card != nil {
			rt.Status.Card = nil
		}
		return
	}

	changeKey := r.workloadChangeKey(ctx, rt)
	annotations := rt.GetAnnotations()
	lastHash := ""
	if annotations != nil {
		lastHash = annotations[AnnotationLastCardFetchHash]
	}
	if changeKey != "" && changeKey == lastHash && rt.Status.Card != nil {
		return
	}

	if ready, msg := r.checkWorkloadReady(ctx, rt.Namespace, rt.Spec.TargetRef); !ready {
		logger.V(1).Info("Workload not ready for card discovery", "reason", msg)
		return
	}

	svc, port, err := r.resolveServiceForWorkload(ctx, rt.Namespace, rt.Spec.TargetRef)
	if err != nil {
		logger.V(1).Info("Service resolution failed for card discovery", "error", err)
		return
	}

	protocol := agentcard.A2AProtocol
	cardData, fetchResult, transportSecurity, err := r.fetchCard(ctx, rt, svc, port, protocol)
	if err != nil {
		logger.Error(err, "Card fetch failed", "workload", rt.Spec.TargetRef.Name)
		return
	}

	newCardHash := computeCardContentHash(cardData)

	cardStatus := &agentv1alpha1.CardStatus{
		AgentCardData:     *cardData,
		CardHash:          newCardHash,
		Protocol:          protocol,
		TransportSecurity: transportSecurity,
	}

	if rt.Status.Card != nil && rt.Status.Card.CardHash == newCardHash {
		cardStatus.LastCardFetchTime = rt.Status.Card.LastCardFetchTime
	} else {
		now := metav1.Now()
		cardStatus.LastCardFetchTime = &now
	}

	if fetchResult != nil && fetchResult.AgentSpiffeID != "" {
		cardStatus.AttestedAgentSpiffeID = fetchResult.AgentSpiffeID
	}

	if r.SignatureProvider != nil && len(cardData.Signatures) > 0 {
		vr, verifyErr := r.SignatureProvider.VerifySignature(ctx, cardData, cardData.Signatures)
		if verifyErr != nil {
			logger.Error(verifyErr, "Signature verification infrastructure error")
			cardStatus.SignatureVerificationDetails = verifyErr.Error()
		} else if vr != nil {
			cardStatus.ValidSignature = &vr.Verified
			cardStatus.SignatureKeyID = vr.KeyID
			cardStatus.SignatureVerificationDetails = vr.Details
		}
	}

	rt.Status.Card = cardStatus

	r.persistCardFetchAnnotation(ctx, rt, changeKey)
}

// persistCardFetchAnnotation writes the change-detection annotation to the
// AgentRuntime's metadata via a patch. Status().Update only persists the status
// subresource, so annotations must be written separately.
//
// Patch refreshes rt from the API server response, which overwrites any
// in-memory status mutations (conditions, card data) that have not yet been
// persisted via Status().Update. We save and restore the status to prevent this.
func (r *AgentRuntimeReconciler) persistCardFetchAnnotation(ctx context.Context, rt *agentv1alpha1.AgentRuntime, changeKey string) {
	logger := log.FromContext(ctx)

	savedStatus := rt.Status.DeepCopy()

	patch := client.MergeFrom(rt.DeepCopy())
	annotations := rt.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[AnnotationLastCardFetchHash] = changeKey
	rt.SetAnnotations(annotations)
	if err := r.Patch(ctx, rt, patch); err != nil {
		logger.Error(err, "Failed to persist card fetch annotation")
	}

	rt.Status = *savedStatus
}

// fetchCard retrieves the agent card, choosing mTLS or plain HTTP based on
// service port availability and fetcher configuration.
func (r *AgentRuntimeReconciler) fetchCard(
	ctx context.Context, rt *agentv1alpha1.AgentRuntime,
	svc *corev1.Service, port int32, protocol string,
) (*agentv1alpha1.AgentCardData, *agentcard.FetchResult, agentv1alpha1.TransportSecurity, error) {
	logger := log.FromContext(ctx)
	ref := rt.Spec.TargetRef

	if r.AuthenticatedFetcher != nil {
		tlsPort := getAgentTLSPort(svc)
		if tlsPort > 0 {
			secureURL := agentcard.GetSecureServiceURL(svc.Name, rt.Namespace, tlsPort)
			fetchResult, err := r.AuthenticatedFetcher.FetchAuthenticated(ctx, protocol, secureURL)
			if err != nil {
				return nil, nil, "", fmt.Errorf("authenticated fetch failed for %s: %w", ref.Name, err)
			}
			if fetchResult.CardData == nil {
				return nil, nil, "", fmt.Errorf("authenticated fetch returned nil card data for %s", ref.Name)
			}
			return fetchResult.CardData, fetchResult, agentv1alpha1.TransportSecurityMTLS, nil
		}
		logger.Info("TLS port not found, falling back to HTTP fetch",
			"service", svc.Name, "expectedPortName", AgentTLSPortName)
		if r.Recorder != nil {
			r.Recorder.Eventf(rt, nil, corev1.EventTypeWarning, "FallbackToHTTP", "FetchCard",
				"Service %s has no %s port; fetch is unverified", svc.Name, AgentTLSPortName)
		}
	}

	if r.AgentFetcher == nil {
		return nil, nil, "", fmt.Errorf("no fetcher configured for card discovery")
	}

	serviceURL := agentcard.GetServiceURL(svc.Name, rt.Namespace, port)
	fetchResult, err := r.AgentFetcher.Fetch(ctx, protocol, serviceURL, ref.Name, rt.Namespace)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetch failed for %s: %w", ref.Name, err)
	}
	if fetchResult.CardData == nil {
		return nil, nil, "", fmt.Errorf("fetch returned nil card data for %s", ref.Name)
	}
	return fetchResult.CardData, fetchResult, agentv1alpha1.TransportSecurityHTTP, nil
}

// workloadChangeKey returns a string that changes when the workload's pod
// template changes. For Deployments this is the observed generation;
// for StatefulSets and Sandboxes it is the resource generation.
func (r *AgentRuntimeReconciler) workloadChangeKey(ctx context.Context, rt *agentv1alpha1.AgentRuntime) string {
	ref := rt.Spec.TargetRef
	acc, ok := newRuntimePodTemplateAccessor(ref.Kind)
	if !ok {
		return ""
	}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: rt.Namespace}, acc.obj); err != nil {
		return ""
	}
	return strconv.FormatInt(acc.obj.GetGeneration(), 10)
}

func computeCardContentHash(cardData *agentv1alpha1.AgentCardData) string {
	if cardData == nil {
		return ""
	}
	data, err := json.Marshal(cardData)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// templateConfigMapNames lists the well-known ConfigMaps that authbridge sidecars
// require. The Helm chart and backend API create these in agent namespaces; the
// operator copies templates from rossoctl-system for namespaces created by other
// means (GitOps, manual kubectl).
var templateConfigMapNames = []string{
	"authbridge-config",
	"authbridge-runtime-config",
}

// ensureNamespaceConfigMaps copies template ConfigMaps from rossoctl-system to the
// target namespace if they don't already exist. This mirrors the backend's
// ensure_configmap() semantics: create-if-not-exists, preserving user customizations.
func (r *AgentRuntimeReconciler) ensureNamespaceConfigMaps(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	reader := r.uncachedReader()

	for _, name := range templateConfigMapNames {
		existing := &corev1.ConfigMap{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check ConfigMap %s/%s: %w", namespace, name, err)
		}

		template := &corev1.ConfigMap{}
		templateKey := client.ObjectKey{Namespace: ClusterDefaultsNamespace, Name: name}
		if err := reader.Get(ctx, templateKey, template); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(1).Info("Template ConfigMap not found in rossoctl-system, skipping",
					"name", name)
				continue
			}
			return fmt.Errorf("failed to read template ConfigMap %s/%s: %w", ClusterDefaultsNamespace, name, err)
		}

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					LabelManagedBy: LabelManagedByValue,
				},
			},
			Data: template.Data,
		}
		if err := r.Create(ctx, cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("failed to create ConfigMap %s/%s: %w", namespace, name, err)
		}
		logger.Info("Created ConfigMap from template", "namespace", namespace, "name", name)
	}
	return nil
}

// ensureSpiffeHelperConfigMap creates or updates the spiffe-helper-config ConfigMap
// in the target namespace using content from PlatformConfig. Unlike template CMs
// which are create-if-not-exists, this always overwrites because PlatformConfig is
// the single source of truth.
func (r *AgentRuntimeReconciler) ensureSpiffeHelperConfigMap(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	cfg := r.getPlatformConfig()

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpiffeHelperConfigMapName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		Data: map[string]string{
			"helper.conf": cfg.Spiffe.HelperConfig,
		},
	}

	existing := &corev1.ConfigMap{}
	err := r.uncachedReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: SpiffeHelperConfigMapName}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("failed to create spiffe-helper-config in %s: %w", namespace, err)
		}
		logger.Info("Created spiffe-helper-config from PlatformConfig", "namespace", namespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check spiffe-helper-config in %s: %w", namespace, err)
	}

	needsUpdate := existing.Data["helper.conf"] != cfg.Spiffe.HelperConfig

	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	if existing.Labels[LabelManagedBy] != LabelManagedByValue {
		existing.Labels[LabelManagedBy] = LabelManagedByValue
		needsUpdate = true
	}

	if !needsUpdate {
		return nil
	}

	existing.Data = desired.Data
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update spiffe-helper-config in %s: %w", namespace, err)
	}
	logger.Info("Updated spiffe-helper-config from PlatformConfig", "namespace", namespace)
	return nil
}

const (
	sccClusterRoleName = "system:openshift:scc:rossoctl-authbridge"
	sccRoleBindingName = "agent-authbridge-scc"
)

// ensureNamespaceSCCBinding creates a RoleBinding in the target namespace that
// grants all ServiceAccounts access to the rossoctl-authbridge SCC via the
// system:openshift:scc:rossoctl-authbridge ClusterRole. On non-OpenShift clusters
// (ClusterRole absent), this is a silent no-op.
func (r *AgentRuntimeReconciler) ensureNamespaceSCCBinding(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	reader := r.uncachedReader()

	// Check if the SCC ClusterRole exists — if not, skip (non-OpenShift).
	cr := &rbacv1.ClusterRole{}
	if err := reader.Get(ctx, client.ObjectKey{Name: sccClusterRoleName}, cr); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("SCC ClusterRole not found, skipping SCC RoleBinding",
				"clusterRole", sccClusterRoleName)
			return nil
		}
		return fmt.Errorf("failed to check SCC ClusterRole %s: %w", sccClusterRoleName, err)
	}

	// Check if the RoleBinding already exists.
	existing := &rbacv1.RoleBinding{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sccRoleBindingName}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check RoleBinding %s/%s: %w", namespace, sccRoleBindingName, err)
	}

	// Create the RoleBinding.
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sccRoleBindingName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     sccClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     rbacv1.GroupKind,
				APIGroup: rbacv1.GroupName,
				Name:     "system:serviceaccounts:" + namespace,
			},
		},
	}
	if err := r.Create(ctx, rb); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create SCC RoleBinding %s/%s: %w", namespace, sccRoleBindingName, err)
	}
	logger.Info("Created SCC RoleBinding", "namespace", namespace, "name", sccRoleBindingName)
	return nil
}

func (r *AgentRuntimeReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// mapWorkloadToAgentRuntime maps workload events to AgentRuntime reconcile requests.
func (r *AgentRuntimeReconciler) mapWorkloadToAgentRuntime(apiVersion, kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		rtList := &agentv1alpha1.AgentRuntimeList{}
		if err := r.List(ctx, rtList, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}

		var requests []reconcile.Request
		for _, rt := range rtList.Items {
			if rt.Spec.TargetRef.Name == obj.GetName() &&
				rt.Spec.TargetRef.Kind == kind &&
				rt.Spec.TargetRef.APIVersion == apiVersion {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      rt.Name,
						Namespace: rt.Namespace,
					},
				})
			}
		}
		return requests
	}
}

// mapClusterConfigMapToAgentRuntimes maps changes to cluster-level ConfigMaps
// (rossoctl-webhook-defaults and rossoctl-webhook-feature-gates) to all AgentRuntime
// reconcile requests across all namespaces.
func (r *AgentRuntimeReconciler) mapClusterConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != ClusterDefaultsNamespace {
		return nil
	}
	if obj.GetName() != ClusterDefaultsConfigMapName && obj.GetName() != ClusterFeatureGatesConfigMapName {
		return nil
	}

	rtList := &agentv1alpha1.AgentRuntimeList{}
	if err := r.List(ctx, rtList); err != nil {
		return nil
	}
	return agentRuntimesToRequests(rtList.Items)
}

// mapNamespaceConfigMapToAgentRuntimes maps changes to relevant
// namespace-scoped ConfigMaps to AgentRuntimes in the same namespace.
// Two ConfigMap shapes are watched:
//
//  1. Namespace defaults — labeled rossoctl.io/defaults=true. Folded into
//     resolvedConfig.Defaults during resolveConfig.
//  2. authbridge-runtime-config (matched by name, no label required) —
//     this is the ConfigMap the admission webhook reads at pod creation.
//     Editing it should trigger a rollout of every AgentRuntime in the
//     namespace because the per-agent ConfigMap is rebuilt from this
//     content on every pod admission.
//
// Both signals enqueue every AgentRuntime in the namespace; the
// reconciler's hash check filters out no-op cases (only AgentRuntimes
// whose computed hash actually changed re-stamp the pod template).
func (r *AgentRuntimeReconciler) mapNamespaceConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	// goconst flags this literal "true" and suggests reusing
	// AnnotationRestartPendingValue, but that constant is semantically a
	// restart-pending marker, not a generic label-true value — reusing it
	// would obscure intent.
	isNsDefaults := labels[LabelNamespaceDefaults] == "true" //nolint:goconst
	isAuthBridgeRuntime := obj.GetName() == AuthBridgeRuntimeConfigMapName

	if !isNsDefaults && !isAuthBridgeRuntime {
		return nil
	}

	rtList := &agentv1alpha1.AgentRuntimeList{}
	if err := r.List(ctx, rtList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	return agentRuntimesToRequests(rtList.Items)
}

// agentRuntimesToRequests converts a list of AgentRuntimes to reconcile requests.
// Returns nil if the list is empty.
func agentRuntimesToRequests(items []agentv1alpha1.AgentRuntime) []reconcile.Request {
	if len(items) == 0 {
		return nil
	}
	requests := make([]reconcile.Request, len(items))
	for i, rt := range items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      rt.Name,
				Namespace: rt.Namespace,
			},
		}
	}
	return requests
}

// mapConfigMapToAgentRuntimes dispatches ConfigMap events to either the cluster
// or namespace mapper based on the ConfigMap's location and labels.
func (r *AgentRuntimeReconciler) mapConfigMapToAgentRuntimes(ctx context.Context, obj client.Object) []reconcile.Request {
	// Check cluster-level ConfigMaps first
	if requests := r.mapClusterConfigMapToAgentRuntimes(ctx, obj); requests != nil {
		return requests
	}
	// Then namespace-level defaults
	return r.mapNamespaceConfigMapToAgentRuntimes(ctx, obj)
}

// SandboxCRDExists checks whether the agent-sandbox CRD is installed on the cluster.
func SandboxCRDExists(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	resources, err := dc.ServerResourcesForGroupVersion("agents.x-k8s.io/v1alpha1")
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == KindSandbox {
			return true
		}
	}
	return false
}

// SetupWithManager registers the AgentRuntime controller with the manager.
func (r *AgentRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentRuntime{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("apps/v1", "Deployment")),
		).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("apps/v1", "StatefulSet")),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.mapConfigMapToAgentRuntimes),
		)

	if SandboxCRDExists(mgr.GetConfig()) {
		sandboxObj := &unstructured.Unstructured{}
		sandboxObj.SetGroupVersionKind(sandboxGVK)
		builder = builder.Watches(
			sandboxObj,
			handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToAgentRuntime("agents.x-k8s.io/v1alpha1", KindSandbox)),
		)
	}

	return builder.
		Named("agentruntime").
		Complete(r)
}

// runtimePodTemplateAccessor provides uniform access to PodTemplateSpec
// labels, annotations, and PodSpec across Deployment and StatefulSet.
type runtimePodTemplateAccessor struct {
	obj               client.Object
	getPodLabels      func(client.Object) map[string]string
	setPodLabels      func(client.Object, map[string]string)
	getPodAnnotations func(client.Object) map[string]string
	setPodAnnotations func(client.Object, map[string]string)
	getPodSpec        func(client.Object) *corev1.PodSpec
}

func newRuntimePodTemplateAccessor(kind string) (*runtimePodTemplateAccessor, bool) {
	switch kind {
	case "Deployment":
		return &runtimePodTemplateAccessor{
			obj: &appsv1.Deployment{},
			getPodLabels: func(o client.Object) map[string]string {
				return o.(*appsv1.Deployment).Spec.Template.Labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				o.(*appsv1.Deployment).Spec.Template.Labels = l
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				return o.(*appsv1.Deployment).Spec.Template.Annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				o.(*appsv1.Deployment).Spec.Template.Annotations = a
			},
			getPodSpec: func(o client.Object) *corev1.PodSpec {
				return &o.(*appsv1.Deployment).Spec.Template.Spec
			},
		}, true
	case "StatefulSet":
		return &runtimePodTemplateAccessor{
			obj: &appsv1.StatefulSet{},
			getPodLabels: func(o client.Object) map[string]string {
				return o.(*appsv1.StatefulSet).Spec.Template.Labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				o.(*appsv1.StatefulSet).Spec.Template.Labels = l
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				return o.(*appsv1.StatefulSet).Spec.Template.Annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				o.(*appsv1.StatefulSet).Spec.Template.Annotations = a
			},
			getPodSpec: func(o client.Object) *corev1.PodSpec {
				return &o.(*appsv1.StatefulSet).Spec.Template.Spec
			},
		}, true
	case KindSandbox:
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(sandboxGVK)
		return &runtimePodTemplateAccessor{
			obj: u,
			getPodLabels: func(o client.Object) map[string]string {
				u := o.(*unstructured.Unstructured)
				labels, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "labels")
				return labels
			},
			setPodLabels: func(o client.Object, l map[string]string) {
				u := o.(*unstructured.Unstructured)
				_ = unstructured.SetNestedStringMap(u.Object, l, "spec", "podTemplate", "metadata", "labels")
			},
			getPodAnnotations: func(o client.Object) map[string]string {
				u := o.(*unstructured.Unstructured)
				annotations, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "annotations")
				return annotations
			},
			setPodAnnotations: func(o client.Object, a map[string]string) {
				u := o.(*unstructured.Unstructured)
				_ = unstructured.SetNestedStringMap(u.Object, a, "spec", "podTemplate", "metadata", "annotations")
			},
			// getPodSpec is not implemented for Sandbox: extracting a typed
			// *corev1.PodSpec out of the unstructured podTemplate isn't needed
			// today (the only former caller, OCI skill mounting, was removed).
			// Return nil rather than leaving the func nil so a future caller hits
			// a nil-check, not a nil-function-call panic.
			getPodSpec: func(client.Object) *corev1.PodSpec { return nil },
		}, true
	default:
		return nil, false
	}
}
