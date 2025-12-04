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
	isCanceled        bool
	client            client.Client
	logger            logr.Logger
	updateFuncs       []UpdateFunc
	updateStatusFuncs []UpdateStatusFunc

	enableAggressiveConflictResolution bool
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
// In case of a Conflict error, the update is not retried by default because the
// underlying resource has been modified in the meantime, and the reconciliation loop
// needs to be restarted anew. However, when aggressive conflict resolution is enabled,
// the updater attempts to refresh the object from the cluster and retry if it's safe
// to do so (e.g., when only the status has changed).
//
// A NotFound error means that the object has been deleted, and the reconciliation loop
// needs to be restarted anew as well.
func retryOnRetryableUpdateError(backoff wait.Backoff, f func(attemptNum uint) error) error {
	var attemptNum uint = 1
	countingF := func() error {
		err := f(attemptNum)
		attemptNum++
		return err
	}
	return retry.OnError(backoff, isRetryableUpdateError, countingF)
}

func (u *Updater) Apply(ctx context.Context, baseObj *unstructured.Unstructured) error {
	if u.isCanceled {
		return nil
	}

	backoff := retry.DefaultRetry

	// Always update the status first. During uninstall, if
	// we remove the finalizer, updating the status will fail
	// because the object and its status will be garbage-collected.
	err := retryOnRetryableUpdateError(backoff, func(attemptNumber uint) error {
		// Note that st will also include all status conditions, also those not managed by helm-operator.
		obj := baseObj.DeepCopy()
		st := statusFor(obj)
		needsStatusUpdate := false
		for _, f := range u.updateStatusFuncs {
			needsStatusUpdate = f(st) || needsStatusUpdate
		}

		if !needsStatusUpdate {
			return nil
		}
		st.updateStatusObject()
		obj.Object["status"] = st.StatusObject

		if attemptNumber > 1 {
			u.logger.V(1).Info("Retrying status update", "attempt", attemptNumber)
		}
		updateErr := u.client.Status().Update(ctx, obj)
		if errors.IsConflict(updateErr) && u.enableAggressiveConflictResolution {
			u.logger.V(1).Info("Status update conflict detected")
			resolved, resolveErr := u.tryRefresh(ctx, baseObj, isSafeForUpdate)
			if resolveErr != nil {
				return resolveErr
			}
			if !resolved {
				return updateErr
			}
			return fmt.Errorf("status update conflict") // retriable error.
		} else if updateErr != nil {
			return updateErr
		}
		baseObj.Object = obj.Object
		return nil
	})
	if err != nil {
		return err
	}

	err = retryOnRetryableUpdateError(backoff, func(attemptNumber uint) error {
		obj := baseObj.DeepCopy()
		needsUpdate := false
		for _, f := range u.updateFuncs {
			needsUpdate = f(obj) || needsUpdate
		}
		if !needsUpdate {
			return nil
		}
		if attemptNumber > 1 {
			u.logger.V(1).Info("Retrying update", "attempt", attemptNumber)
		}
		updateErr := u.client.Update(ctx, obj)
		if errors.IsConflict(updateErr) && u.enableAggressiveConflictResolution {
			u.logger.V(1).Info("Update conflict detected")
			resolved, resolveErr := u.tryRefresh(ctx, baseObj, isSafeForUpdate)
			if resolveErr != nil {
				return resolveErr
			}
			if !resolved {
				return updateErr
			}
			return fmt.Errorf("update conflict due to externally-managed status conditions") // retriable error.
		} else if updateErr != nil {
			return updateErr
		}
		baseObj.Object = obj.Object
		return nil
	})

	return err
}

func isSafeForUpdate(logger logr.Logger, inMemory *unstructured.Unstructured, onCluster *unstructured.Unstructured) bool {
	// Compare metadata (excluding resourceVersion).
	inMemoryMetadata := metadataWithoutResourceVersion(inMemory)
	onClusterMetadata := metadataWithoutResourceVersion(onCluster)
	if !reflect.DeepEqual(inMemoryMetadata, onClusterMetadata) {
		// Diff in generation. Nothing we can do about it -> Fail.
		logger.V(1).Info("Not refreshing object due to generation mismatch",
			"namespace", inMemory.GetNamespace(),
			"name", inMemory.GetName(),
			"gkv", inMemory.GroupVersionKind(),
		)
		return false
	}
	// Extra verification to make sure that the spec has not changed.
	if !reflect.DeepEqual(inMemory.Object["spec"], onCluster.Object["spec"]) {
		// Diff in object spec. Nothing we can do about it -> Fail.
		logger.V(1).Info("Not refreshing object due to spec mismatch",
			"namespace", inMemory.GetNamespace(),
			"name", inMemory.GetName(),
			"gkv", inMemory.GroupVersionKind(),
		)
		return false
	}
	return true
}

func metadataWithoutResourceVersion(u *unstructured.Unstructured) map[string]interface{} {
	metadata, ok := u.Object["metadata"].(map[string]interface{})
	if !ok {
		return nil
	}
	modifiedMetadata := make(map[string]interface{}, len(metadata))
	for k, v := range metadata {
		if k == "resourceVersion" {
			continue
		}
		modifiedMetadata[k] = v
	}
	return modifiedMetadata
}

func (u *Updater) tryRefresh(ctx context.Context, obj *unstructured.Unstructured, isSafe func(logger logr.Logger, inMemory *unstructured.Unstructured, onCluster *unstructured.Unstructured) bool) (bool, error) {
	// Re-fetch object with client.
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	objectKey := client.ObjectKeyFromObject(obj)
	if err := u.client.Get(ctx, objectKey, current); err != nil {
		err = fmt.Errorf("refreshing object %s/%s: %w", objectKey.Namespace, objectKey.Name, err)
		return false, err
	}

	if !isSafe(u.logger, obj, current) {
		return false, nil
	}

	obj.Object = current.Object
	u.logger.V(1).Info("Refreshed object",
		"namespace", objectKey.Namespace,
		"name", objectKey.Name,
		"gvk", obj.GroupVersionKind())
	return true, nil
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
