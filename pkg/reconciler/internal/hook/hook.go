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

package hook

import (
	"sync"

	"github.com/go-logr/logr"
	sdkhandler "github.com/operator-framework/operator-lib/handler"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"

	"github.com/joelanford/helm-operator/pkg/hook"
	"github.com/joelanford/helm-operator/pkg/internal/sdk/controllerutil"
	"github.com/joelanford/helm-operator/pkg/internal/sdk/predicate"
	"github.com/joelanford/helm-operator/pkg/manifestutil"
)

func NewDependentResourceWatcher(c controller.Controller, rm meta.RESTMapper, watchReleaseResources bool, extraWatches ...schema.GroupVersionKind) hook.PostHook {
	return &dependentResourceWatcher{
		controller:            c,
		restMapper:            rm,
		watchReleaseResources: watchReleaseResources,
		extraWatches:          extraWatches,
		m:                     sync.Mutex{},
		watches:               make(map[schema.GroupVersionKind]struct{}),
	}
}

type dependentResourceWatcher struct {
	controller            controller.Controller
	restMapper            meta.RESTMapper
	watchReleaseResources bool
	extraWatches          []schema.GroupVersionKind

	m       sync.Mutex
	watches map[schema.GroupVersionKind]struct{}
}

func (d *dependentResourceWatcher) Exec(owner *unstructured.Unstructured, rel release.Release, log logr.Logger) error {
	// using predefined functions for filtering events
	dependentPredicate := predicate.DependentPredicateFuncs()

	var allWatches []unstructured.Unstructured
	if d.watchReleaseResources {
		resources := releaseutil.SplitManifests(rel.Manifest)
		for _, r := range resources {
			var obj unstructured.Unstructured
			err := yaml.Unmarshal([]byte(r), &obj)
			if err != nil {
				return err
			}

			allWatches = append(allWatches, obj)
		}
	}

	// For extra watches, we only support same namespace
	for _, extraWatchGVK := range d.extraWatches {
		var obj unstructured.Unstructured
		obj.SetGroupVersionKind(extraWatchGVK)
		obj.SetNamespace(owner.GetNamespace())
		allWatches = append(allWatches, obj)
	}

	d.m.Lock()
	defer d.m.Unlock()
	for _, obj := range allWatches {
		depGVK := obj.GroupVersionKind()
		if _, ok := d.watches[depGVK]; ok || depGVK.Empty() {
			continue
		}

		useOwnerRef, err := controllerutil.SupportsOwnerReference(d.restMapper, owner, &obj)
		if err != nil {
			return err
		}

		if useOwnerRef && !manifestutil.HasResourcePolicyKeep(obj.GetAnnotations()) {
			if err := d.controller.Watch(&source.Kind{Type: &obj}, &handler.EnqueueRequestForOwner{
				OwnerType:    owner,
				IsController: true,
			}, dependentPredicate); err != nil {
				return err
			}
		} else {
			if err := d.controller.Watch(&source.Kind{Type: &obj}, &sdkhandler.EnqueueRequestForAnnotation{
				Type: owner.GetObjectKind().GroupVersionKind().GroupKind(),
			}, dependentPredicate); err != nil {
				return err
			}
		}

		d.watches[depGVK] = struct{}{}
		log.V(1).Info("Watching dependent resource", "dependentAPIVersion", depGVK.GroupVersion(), "dependentKind", depGVK.Kind)
	}
	return nil
}
