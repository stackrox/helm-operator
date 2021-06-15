package extensions

import (
	"context"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ReconcileExtension is an arbitrary extension that can be implemented to run either before
// or after the main Helm reconciliation action.
// An error returned by a ReconcileExtension will cause the Reconcile to fail, unlike a hook error.
type ReconcileExtension func(context.Context, *unstructured.Unstructured, logr.Logger) error
