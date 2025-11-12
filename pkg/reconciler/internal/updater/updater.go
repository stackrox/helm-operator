/*
Copyright 2020 The Operator-SDK Authors.

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

package updater

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/operator-framework/helm-operator-plugins/internal/sdk/controllerutil"
	"github.com/operator-framework/helm-operator-plugins/pkg/extensions"
	"github.com/operator-framework/helm-operator-plugins/pkg/internal/status"
)

func New(client client.Client, logger logr.Logger) Updater {
	logger = logger.WithName("updater")
	return Updater{
		client: client,
		logger: logger,
	}
}

type Updater struct {
	isCanceled                         bool
	client                             client.Client
	logger                             logr.Logger
	updateFuncs                        []UpdateFunc
	updateStatusFuncs                  []UpdateStatusFunc
	externallyManagedStatusConditions  map[string]struct{}
	enableAggressiveConflictResolution bool
}

func (u *Updater) RegisterExternallyManagedStatusConditions(conditions map[string]struct{}) {
	if u.externallyManagedStatusConditions == nil {
		u.externallyManagedStatusConditions = make(map[string]struct{}, len(conditions))
	}
	for conditionType := range conditions {
		u.externallyManagedStatusConditions[conditionType] = struct{}{}
	}
}

func (u *Updater) EnableAggressiveConflictResolution() {
	u.enableAggressiveConflictResolution = true
}

type UpdateFunc func(*unstructured.Unstructured) bool
type UpdateStatusFunc func(*helmAppStatus) bool

func (u *Updater) Update(fs ...UpdateFunc) {
	u.updateFuncs = append(u.updateFuncs, fs...)
}

func (u *Updater) UpdateStatus(fs ...UpdateStatusFunc) {
	u.updateStatusFuncs = append(u.updateStatusFuncs, fs...)
}

func (u *Updater) UpdateStatusCustom(f extensions.UpdateStatusFunc) {
	updateFn := func(status *helmAppStatus) bool {
		status.updateStatusObject()

		unstructuredStatus := unstructured.Unstructured{Object: status.StatusObject}
		if !f(&unstructuredStatus) {
			return false
		}
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredStatus.Object, status)
		status.StatusObject = unstructuredStatus.Object
		return true
	}
	u.UpdateStatus(updateFn)
}

func (u *Updater) CancelUpdates() {
	u.isCanceled = true
}

func isRetryableUpdateError(err error) bool {
	return !errors.IsConflict(err) && !errors.IsNotFound(err)
}

// retryOnRetryableUpdateError retries the given function until it succeeds,
// until the given backoff is exhausted, or until the error is not retryable.
//
// In case of a Conflict error, the update cannot be retried because the underlying
// resource has been modified in the meantime, and the reconciliation loop needs
// to be restarted anew.
//
// A NotFound error means that the object has been deleted, and the reconciliation loop
// needs to be restarted anew as well.
func retryOnRetryableUpdateError(backoff wait.Backoff, f func() error) error {
	return retry.OnError(backoff, isRetryableUpdateError, f)
}

func (u *Updater) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	if u.isCanceled {
		return nil
	}

	backoff := retry.DefaultRetry

	st := statusFor(obj)
	needsStatusUpdate := false
	for _, f := range u.updateStatusFuncs {
		needsStatusUpdate = f(st) || needsStatusUpdate
	}

	// Always update the status first. During uninstall, if
	// we remove the finalizer, updating the status will fail
	// because the object and its status will be garbage-collected.
	if needsStatusUpdate {
		st.updateStatusObject()
		obj.Object["status"] = st.StatusObject
		if err := retryOnRetryableUpdateError(backoff, func() error {
			updateErr := u.client.Status().Update(ctx, obj)
			if errors.IsConflict(updateErr) && u.enableAggressiveConflictResolution {
				u.logger.V(1).Info("Status update conflict detected")
				resolved, resolveErr := u.tryRefreshObject(ctx, obj)
				u.logger.V(1).Info("tryRefreshObject", "resolved", resolved, "resolveErr", resolveErr)
				if resolveErr != nil {
					return resolveErr
				}
				if !resolved {
					return updateErr
				}
				return fmt.Errorf("status update conflict due to externally-managed status conditions") // retriable error.
			}
			return updateErr
		}); err != nil {
			return err
		}
	}

	needsUpdate := false
	for _, f := range u.updateFuncs {
		needsUpdate = f(obj) || needsUpdate
	}
	if needsUpdate {
		if err := retryOnRetryableUpdateError(backoff, func() error {
			updateErr := u.client.Update(ctx, obj)
			if errors.IsConflict(updateErr) && u.enableAggressiveConflictResolution {
				u.logger.V(1).Info("Status update conflict detected")
				resolved, resolveErr := u.tryRefreshObject(ctx, obj)
				u.logger.V(1).Info("tryRefreshObject", "resolved", resolved, "resolveErr", resolveErr)
				if resolveErr != nil {
					return resolveErr
				}
				if !resolved {
					return updateErr
				}
				return fmt.Errorf("update conflict due to externally-managed status conditions") // retriable error.
			}
			return updateErr
		}); err != nil {
			return err
		}
	}
	return nil
}

// This function tries to merge the status of obj with the current version of the status on the cluster.
// The unstructured obj is expected to have been modified and to have caused a conflict error during an update attempt.
// If the only differences between obj and the current version are in externally managed status conditions,
// those conditions are merged from the current version into obj.
// Returns true if updating shall be retried with the updated obj.
// Returns false if the conflict could not be resolved.
func (u *Updater) tryRefreshObject(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	// Retrieve current version from the cluster.
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	objectKey := client.ObjectKeyFromObject(obj)
	if err := u.client.Get(ctx, objectKey, current); err != nil {
		err = fmt.Errorf("refreshing object %s/%s: %w", objectKey.Namespace, objectKey.Name, err)
		return false, err
	}

	if !reflect.DeepEqual(obj.Object["spec"], current.Object["spec"]) {
		// Diff in object spec. Nothing we can do about it -> Fail.
		u.logger.V(1).Info("Cluster resource cannot be updated due to spec mismatch",
			"namespace", objectKey.Namespace, "name", objectKey.Name, "gkv", obj.GroupVersionKind(),
		)
		return false, nil
	}

	// Merge externally managed conditions from current into object copy.
	objCopy := obj.DeepCopy()
	u.mergeExternallyManagedConditions(objCopy, current)

	// Overwrite metadata with the most recent in-cluster version.
	// This ensures we have the latest resourceVersion, annotations, labels, etc.
	// The reconciler flow can modify the set of finalizers, hence we need to preserve them.
	newMetadata, metadataFound, _ := unstructured.NestedFieldNoCopy(current.Object, "metadata")
	if !metadataFound {
		return false, fmt.Errorf("failed to obtain metadata from current object")
	}
	finalizers, finalizersFound, _ := unstructured.NestedFieldNoCopy(obj.Object, "metadata", "finalizers")
	if finalizersFound {
		err := unstructured.SetNestedField(newMetadata.(map[string]interface{}), finalizers, "finalizers")
		if err != nil {
			return false, fmt.Errorf("failed to set finalizers in merged metadata: %w", err)
		}
	}
	objCopy.Object["metadata"] = newMetadata

	// We were able to resolve the conflict by merging external conditions.
	obj.Object = objCopy.Object

	u.logger.V(1).Info("Resolved update conflict by merging externally-managed status conditions")
	return true, nil
}

// mergeExternallyManagedConditions updates obj's status conditions by replacing
// externally managed conditions with their values from current.
// Uses current's ordering to avoid false positives in conflict detection.
func (u *Updater) mergeExternallyManagedConditions(obj, current *unstructured.Unstructured) {
	objConditions := statusConditionsFromObject(obj)
	if objConditions == nil {
		return
	}

	currentConditions := statusConditionsFromObject(current)
	if currentConditions == nil {
		return
	}

	// Build a map of all conditions from obj (by type).
	objConditionsByType := make(map[string]map[string]interface{})
	for _, cond := range objConditions {
		if condType, ok := cond["type"].(string); ok {
			objConditionsByType[condType] = cond
		}
	}

	// Build merged conditions starting from current's ordering.
	mergedConditions := make([]map[string]interface{}, 0, len(currentConditions))
	for _, cond := range currentConditions {
		condType, ok := cond["type"].(string)
		if !ok {
			// Shouldn't happen.
			continue
		}
		if _, isExternal := u.externallyManagedStatusConditions[condType]; isExternal {
			// Keep external condition from current.
			mergedConditions = append(mergedConditions, cond)
		} else if objCond, found := objConditionsByType[condType]; found {
			// Replace with non-external condition from obj.
			mergedConditions = append(mergedConditions, objCond)
			delete(objConditionsByType, condType) // Mark as used.
		}
		// Note: If condition exists in current but not in obj (and is non-external),
		// we skip it.
	}

	// Add any remaining non-externally managed conditions from obj that weren't in current.
	for condType, cond := range objConditionsByType {
		if _, isExternal := u.externallyManagedStatusConditions[condType]; isExternal {
			continue
		}
		mergedConditions = append(mergedConditions, cond)
	}

	// Convert to []interface{} for SetNestedField
	mergedConditionsInterface := make([]interface{}, len(mergedConditions))
	for i, cond := range mergedConditions {
		mergedConditionsInterface[i] = cond
	}

	// Write the modified conditions back.
	_ = unstructured.SetNestedField(obj.Object, mergedConditionsInterface, "status", "conditions")
}

// statusConditionsFromObject extracts status conditions from an unstructured object.
// Returns nil if the conditions field is not found or is not the expected type.
func statusConditionsFromObject(obj *unstructured.Unstructured) []map[string]interface{} {
	conditionsRaw, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	if !ok {
		return nil
	}

	conditionsSlice, ok := conditionsRaw.([]interface{})
	if !ok {
		return nil
	}

	// Convert []interface{} to []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(conditionsSlice))
	for _, cond := range conditionsSlice {
		if condMap, ok := cond.(map[string]interface{}); ok {
			result = append(result, condMap)
		}
	}
	return result
}

func RemoveFinalizer(finalizer string) UpdateFunc {
	return func(obj *unstructured.Unstructured) bool {
		if !controllerutil.ContainsFinalizer(obj, finalizer) {
			return false
		}
		controllerutil.RemoveFinalizer(obj, finalizer)
		return true
	}
}

func EnsureCondition(condition status.Condition) UpdateStatusFunc {
	return func(status *helmAppStatus) bool {
		return status.Conditions.SetCondition(condition)
	}
}

func EnsureConditionUnknown(t status.ConditionType) UpdateStatusFunc {
	return func(s *helmAppStatus) bool {
		return s.Conditions.SetCondition(status.Condition{
			Type:   t,
			Status: corev1.ConditionUnknown,
		})
	}
}

func EnsureConditionAbsent(t status.ConditionType) UpdateStatusFunc {
	return func(status *helmAppStatus) bool {
		return status.Conditions.RemoveCondition(t)
	}
}

func EnsureDeployedRelease(rel *release.Release) UpdateStatusFunc {
	return func(status *helmAppStatus) bool {
		newRel := helmAppReleaseFor(rel)
		if status.DeployedRelease == nil && newRel == nil {
			return false
		}
		if status.DeployedRelease != nil && newRel != nil &&
			*status.DeployedRelease == *newRel {
			return false
		}
		status.DeployedRelease = newRel
		return true
	}
}

func RemoveDeployedRelease() UpdateStatusFunc {
	return EnsureDeployedRelease(nil)
}

type helmAppStatus struct {
	StatusObject map[string]interface{} `json:"-"`

	Conditions      status.Conditions `json:"conditions"`
	DeployedRelease *helmAppRelease   `json:"deployedRelease,omitempty"`
}

func (s *helmAppStatus) updateStatusObject() {
	unstructuredHelmAppStatus, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
	if s.StatusObject == nil {
		s.StatusObject = make(map[string]interface{})
	}
	s.StatusObject["conditions"] = unstructuredHelmAppStatus["conditions"]
	if deployedRelease := unstructuredHelmAppStatus["deployedRelease"]; deployedRelease != nil {
		s.StatusObject["deployedRelease"] = deployedRelease
	} else {
		delete(s.StatusObject, "deployedRelease")
	}
}

type helmAppRelease struct {
	Name     string `json:"name,omitempty"`
	Manifest string `json:"manifest,omitempty"`
}

func statusFor(obj *unstructured.Unstructured) *helmAppStatus {
	if obj == nil || obj.Object == nil {
		return nil
	}
	status, ok := obj.Object["status"]
	if !ok {
		return &helmAppStatus{}
	}

	switch s := status.(type) {
	case *helmAppStatus:
		return s
	case helmAppStatus:
		return &s
	case map[string]interface{}:
		out := &helmAppStatus{}
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(s, out)
		out.StatusObject = s
		return out
	default:
		return &helmAppStatus{}
	}
}

func helmAppReleaseFor(rel *release.Release) *helmAppRelease {
	if rel == nil {
		return nil
	}
	return &helmAppRelease{
		Name:     rel.Name,
		Manifest: rel.Manifest,
	}
}
