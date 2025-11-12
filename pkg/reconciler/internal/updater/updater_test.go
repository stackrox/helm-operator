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
	"errors"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/operator-framework/helm-operator-plugins/pkg/reconciler/internal/conditions"
)

const (
	testFinalizer           = "testFinalizer"
	availableReplicasStatus = int64(3)
	replicasStatus          = int64(5)
)

var _ = Describe("Updater", func() {
	var (
		cl               client.Client
		u                Updater
		obj              *unstructured.Unstructured
		interceptorFuncs interceptor.Funcs
	)

	JustBeforeEach(func() {
		cl = fake.NewClientBuilder().WithInterceptorFuncs(interceptorFuncs).Build()
		u = New(cl, logr.Discard())
		obj = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "testDeployment",
				"namespace": "testNamespace",
			},
			"spec": map[string]interface{}{},
		}}
		Expect(cl.Create(context.TODO(), obj)).To(Succeed())
	})

	When("the object does not exist", func() {
		It("should fail", func() {
			Expect(cl.Delete(context.TODO(), obj)).To(Succeed())
			u.Update(func(u *unstructured.Unstructured) bool {
				u.SetAnnotations(map[string]string{"foo": "bar"})
				return true
			})
			err := u.Apply(context.TODO(), obj)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	When("an update is a change", func() {
		var updateCallCount int

		BeforeEach(func() {
			// On the first update of (status) subresource, return an error. After that do what is expected.
			interceptorFuncs.SubResourceUpdate = func(ctx context.Context, interceptorClient client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				updateCallCount++
				if updateCallCount == 1 {
					return errors.New("transient error")
				}
				return interceptorClient.SubResource(subResourceName).Update(ctx, obj, opts...)
			}
		})
		It("should apply an update function", func() {
			u.Update(func(u *unstructured.Unstructured) bool {
				u.SetAnnotations(map[string]string{"foo": "bar"})
				return true
			})
			resourceVersion := obj.GetResourceVersion()

			Expect(u.Apply(context.TODO(), obj)).To(Succeed())
			Expect(cl.Get(context.TODO(), types.NamespacedName{Namespace: "testNamespace", Name: "testDeployment"}, obj)).To(Succeed())
			Expect(obj.GetAnnotations()["foo"]).To(Equal("bar"))
			Expect(obj.GetResourceVersion()).NotTo(Equal(resourceVersion))
		})

		It("should apply an update status function", func() {
			u.UpdateStatus(EnsureCondition(conditions.Deployed(corev1.ConditionTrue, "", "")))
			resourceVersion := obj.GetResourceVersion()

			Expect(u.Apply(context.TODO(), obj)).To(Succeed())
			Expect(cl.Get(context.TODO(), types.NamespacedName{Namespace: "testNamespace", Name: "testDeployment"}, obj)).To(Succeed())
			Expect((obj.Object["status"].(map[string]interface{}))["conditions"]).To(HaveLen(1))
			Expect(obj.GetResourceVersion()).NotTo(Equal(resourceVersion))
		})

		It("should support a mix of standard and custom status updates", func() {
			u.UpdateStatus(EnsureCondition(conditions.Deployed(corev1.ConditionTrue, "", "")))
			u.UpdateStatusCustom(func(uSt *unstructured.Unstructured) bool {
				Expect(unstructured.SetNestedField(uSt.Object, replicasStatus, "replicas")).To(Succeed())
				return true
			})
			u.UpdateStatus(EnsureCondition(conditions.Irreconcilable(corev1.ConditionFalse, "", "")))
			u.UpdateStatusCustom(func(uSt *unstructured.Unstructured) bool {
				Expect(unstructured.SetNestedField(uSt.Object, availableReplicasStatus, "availableReplicas")).To(Succeed())
				return true
			})
			u.UpdateStatus(EnsureCondition(conditions.Initialized(corev1.ConditionTrue, "", "")))

			Expect(u.Apply(context.TODO(), obj)).To(Succeed())
			Expect(cl.Get(context.TODO(), types.NamespacedName{Namespace: "testNamespace", Name: "testDeployment"}, obj)).To(Succeed())
			Expect((obj.Object["status"].(map[string]interface{}))["conditions"]).To(HaveLen(3))
			_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "deployedRelease")
			Expect(found).To(BeFalse())
			Expect(err).To(Not(HaveOccurred()))

			val, found, err := unstructured.NestedInt64(obj.Object, "status", "replicas")
			Expect(val).To(Equal(replicasStatus))
			Expect(found).To(BeTrue())
			Expect(err).To(Not(HaveOccurred()))

			val, found, err = unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
			Expect(val).To(Equal(availableReplicasStatus))
			Expect(found).To(BeTrue())
			Expect(err).To(Not(HaveOccurred()))
		})

		It("should preserve any custom status across multiple apply calls", func() {
			u.UpdateStatusCustom(func(uSt *unstructured.Unstructured) bool {
				Expect(unstructured.SetNestedField(uSt.Object, int64(5), "replicas")).To(Succeed())
				return true
			})
			Expect(u.Apply(context.TODO(), obj)).To(Succeed())

			Expect(cl.Get(context.TODO(), types.NamespacedName{Namespace: "testNamespace", Name: "testDeployment"}, obj)).To(Succeed())

			_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "deployedRelease")
			Expect(found).To(BeFalse())
			Expect(err).To(Not(HaveOccurred()))

			val, found, err := unstructured.NestedInt64(obj.Object, "status", "replicas")
			Expect(val).To(Equal(replicasStatus))
			Expect(found).To(BeTrue())
			Expect(err).To(Succeed())

			u.UpdateStatus(EnsureCondition(conditions.Deployed(corev1.ConditionTrue, "", "")))
			Expect(u.Apply(context.TODO(), obj)).To(Succeed())

			Expect(cl.Get(context.TODO(), types.NamespacedName{Namespace: "testNamespace", Name: "testDeployment"}, obj)).To(Succeed())
			Expect((obj.Object["status"].(map[string]interface{}))["conditions"]).To(HaveLen(1))

			_, found, err = unstructured.NestedFieldNoCopy(obj.Object, "status", "deployedRelease")
			Expect(found).To(BeFalse())
			Expect(err).To(Not(HaveOccurred()))

			val, found, err = unstructured.NestedInt64(obj.Object, "status", "replicas")
			Expect(val).To(Equal(replicasStatus))
			Expect(found).To(BeTrue())
			Expect(err).To(Succeed())
		})
	})
})

var _ = Describe("RemoveFinalizer", func() {
	var obj *unstructured.Unstructured

	BeforeEach(func() {
		obj = &unstructured.Unstructured{}
	})

	It("should remove finalizer if present", func() {
		obj.SetFinalizers([]string{testFinalizer})
		Expect(RemoveFinalizer(testFinalizer)(obj)).To(BeTrue())
		Expect(obj.GetFinalizers()).To(BeEmpty())
	})

	It("should return false if finalizer is not present", func() {
		Expect(RemoveFinalizer(testFinalizer)(obj)).To(BeFalse())
		Expect(obj.GetFinalizers()).To(BeEmpty())
	})
})

var _ = Describe("EnsureCondition", func() {
	var obj *helmAppStatus

	BeforeEach(func() {
		obj = &helmAppStatus{}
	})

	It("should add condition if not present", func() {
		Expect(EnsureCondition(conditions.Deployed(corev1.ConditionTrue, "", ""))(obj)).To(BeTrue())
		Expect(obj.Conditions.IsTrueFor(conditions.TypeDeployed)).To(BeTrue())
	})

	It("should not add duplicate condition", func() {
		obj.Conditions.SetCondition(conditions.Deployed(corev1.ConditionTrue, "", ""))
		Expect(EnsureCondition(conditions.Deployed(corev1.ConditionTrue, "", ""))(obj)).To(BeFalse())
		Expect(obj.Conditions.IsTrueFor(conditions.TypeDeployed)).To(BeTrue())
	})
})

var _ = Describe("EnsureDeployedRelease", func() {
	var obj *helmAppStatus
	var rel *release.Release
	var statusRelease *helmAppRelease

	BeforeEach(func() {
		obj = &helmAppStatus{}
		rel = &release.Release{
			Name:     "initialName",
			Manifest: "initialManifest",
		}
		statusRelease = &helmAppRelease{
			Name:     "initialName",
			Manifest: "initialManifest",
		}
	})

	It("should add deployed release if not present", func() {
		Expect(EnsureDeployedRelease(rel)(obj)).To(BeTrue())
		Expect(obj.DeployedRelease).To(Equal(statusRelease))
	})

	It("should not update identical deployed release", func() {
		obj.DeployedRelease = statusRelease
		Expect(EnsureDeployedRelease(rel)(obj)).To(BeFalse())
		Expect(obj.DeployedRelease).To(Equal(statusRelease))
	})

	It("should update deployed release if different name", func() {
		obj.DeployedRelease = statusRelease
		Expect(EnsureDeployedRelease(&release.Release{Name: "newName", Manifest: "initialManifest"})(obj)).To(BeTrue())
		Expect(obj.DeployedRelease).To(Equal(&helmAppRelease{Name: "newName", Manifest: "initialManifest"}))
	})

	It("should update deployed release if different manifest", func() {
		obj.DeployedRelease = statusRelease
		Expect(EnsureDeployedRelease(&release.Release{Name: "initialName", Manifest: "newManifest"})(obj)).To(BeTrue())
		Expect(obj.DeployedRelease).To(Equal(&helmAppRelease{Name: "initialName", Manifest: "newManifest"}))
	})
})

var _ = Describe("RemoveDeployedRelease", func() {
	var obj *helmAppStatus
	var statusRelease *helmAppRelease

	BeforeEach(func() {
		obj = &helmAppStatus{}
		statusRelease = &helmAppRelease{
			Name:     "initialName",
			Manifest: "initialManifest",
		}
	})

	It("should remove deployed release if present", func() {
		obj.DeployedRelease = statusRelease
		Expect(RemoveDeployedRelease()(obj)).To(BeTrue())
		Expect(obj.DeployedRelease).To(BeNil())
	})

	It("should not update if deployed release is already nil", func() {
		Expect(RemoveDeployedRelease()(obj)).To(BeFalse())
		Expect(obj.DeployedRelease).To(BeNil())
	})
})

var _ = Describe("statusFor", func() {
	var obj *unstructured.Unstructured

	BeforeEach(func() {
		obj = &unstructured.Unstructured{Object: map[string]interface{}{}}
	})

	It("should handle nil", func() {
		obj.Object = nil
		Expect(statusFor(obj)).To(BeNil())

		obj = nil
		Expect(statusFor(obj)).To(BeNil())
	})

	It("should handle status not present", func() {
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{}))
	})

	It("should handle *helmAppsStatus", func() {
		obj.Object["status"] = &helmAppStatus{}
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{}))
	})

	It("should handle helmAppsStatus", func() {
		obj.Object["status"] = helmAppStatus{}
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{}))
	})

	It("should handle map[string]interface{}", func() {
		uSt := map[string]interface{}{}
		obj.Object["status"] = uSt
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{StatusObject: uSt}))
	})

	It("should handle arbitrary types", func() {
		obj.Object["status"] = 10
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{}))

		obj.Object["status"] = "hello"
		Expect(statusFor(obj)).To(Equal(&helmAppStatus{}))
	})
})

var _ = Describe("tryRefreshObject", func() {
	var (
		cl                     client.Client
		u                      Updater
		obj                    *unstructured.Unstructured
		current                *unstructured.Unstructured
		externalCondition      map[string]interface{}
		externalConditionTypes map[string]struct{}
		nonExternalCondition   map[string]interface{}
		// nonExternalConditionB  map[string]interface{}
	)

	BeforeEach(func() {
		externalCondition = map[string]interface{}{
			"type":   "ExternalCondition",
			"status": string(corev1.ConditionTrue),
			"reason": "ExternallyManaged",
		}
		externalConditionTypes = map[string]struct{}{
			externalCondition["type"].(string): {},
		}
		nonExternalCondition = map[string]interface{}{
			"type":   "Deployed",
			"status": string(corev1.ConditionTrue),
			"reason": "InitialDeployment",
		}
		// nonExternalConditionB = map[string]interface{}{
		// 	"type":   "Foo",
		// 	"status": string(corev1.ConditionTrue),
		// 	"reason": "Success",
		// }

		// Setup obj with initial state (version 100).
		obj = &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "example.com/v1",
				"kind":       "TestResource",
				"metadata": map[string]interface{}{
					"name":            "test-obj",
					"namespace":       "default",
					"resourceVersion": "100",
				},
				"status": map[string]interface{}{},
			},
		}

		// Setup current with updated state (version 101).
		current = &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "example.com/v1",
				"kind":       "TestResource",
				"metadata": map[string]interface{}{
					"name":            "test-obj",
					"namespace":       "default",
					"resourceVersion": "101",
				},
				"status": map[string]interface{}{},
			},
		}
	})

	When("finalizers are updated in the reconciler flow", func() {
		BeforeEach(func() {
			err := unstructured.SetNestedStringSlice(obj.Object, []string{"finalizerA, finalizerB"}, "metadata", "finalizers")
			Expect(err).ToNot(HaveOccurred())
			err = unstructured.SetNestedStringSlice(current.Object, []string{"finalizerA, finalizerC"}, "metadata", "finalizers")
			Expect(err).ToNot(HaveOccurred())

			cl = fake.NewClientBuilder().WithObjects(current).Build()
			u = New(cl, logr.Discard())
		})

		It("should preserve these changes", func() {
			resolved, err := u.tryRefreshObject(context.Background(), obj)
			Expect(err).ToNot(HaveOccurred())
			Expect(resolved).To(BeTrue())
			Expect(obj.GetResourceVersion()).To(Equal("101"))

			// Verify finalizers were merged
			finalizers := obj.GetFinalizers()
			Expect(finalizers).To(Equal([]string{"finalizerA, finalizerB"}))
		})
	})

	When("only external conditions differ", func() {
		BeforeEach(func() {
			obj.Object["status"] = map[string]interface{}{
				"conditions": []interface{}{nonExternalCondition},
			}
			current.Object["status"] = map[string]interface{}{
				"conditions": []interface{}{nonExternalCondition, externalCondition},
			}

			cl = fake.NewClientBuilder().WithObjects(current).Build()
			u = New(cl, logr.Discard())
			u.RegisterExternallyManagedStatusConditions(externalConditionTypes)
		})

		It("should merge and return resolved", func() {
			resolved, err := u.tryRefreshObject(context.Background(), obj)

			Expect(err).ToNot(HaveOccurred())
			Expect(resolved).To(BeTrue())
			Expect(obj.GetResourceVersion()).To(Equal("101"))

			// Verify external condition was merged
			conditions, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			Expect(ok).To(BeTrue())
			Expect(conditions).To(HaveLen(2))

			// Verify ordering (Deployed first, ExternalCondition second from current)
			Expect(conditions[0].(map[string]interface{})["type"]).To(Equal("Deployed"))
			Expect(conditions[1].(map[string]interface{})["type"]).To(Equal("ExternalCondition"))
		})
	})

	// When("no external conditions are configured", func() {
	// 	BeforeEach(func() {
	// 		cl = fake.NewClientBuilder().Build()
	// 		u = New(cl, logr.Discard())
	// 	})

	// 	It("should return early without calling Get", func() {
	// 		resolved, err := u.tryRefreshObject(context.Background(), obj)
	// 		Expect(err).ToNot(HaveOccurred())
	// 		Expect(resolved).To(BeFalse())
	// 	})
	// })

	When("Get returns an error", func() {
		BeforeEach(func() {
			// Empty client - Get will return NotFound
			cl = fake.NewClientBuilder().Build()
			u = New(cl, logr.Discard())
			u.RegisterExternallyManagedStatusConditions(externalConditionTypes)
		})

		It("should return the error", func() {
			resolved, err := u.tryRefreshObject(context.Background(), obj)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(resolved).To(BeFalse())
		})
	})
})
