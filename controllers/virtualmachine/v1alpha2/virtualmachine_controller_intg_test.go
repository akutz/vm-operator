// Copyright (c) 2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha2_test

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	vmopv1a2 "github.com/vmware-tanzu/vm-operator/api/v1alpha2"

	"github.com/vmware-tanzu/vm-operator/test/builder"
)

func intgTests() {

	var (
		ctx                  *builder.IntegrationTestContext
		obj                  client.Object
		objKey               types.NamespacedName
		newObjFn             func() client.Object
		getInstanceUUIDFn    func(client.Object) string
		pauseAnnotationLabel string
	)

	BeforeEach(func() {
		ctx = suite.NewIntegrationTestContext()
		objKey = types.NamespacedName{Name: uuid.NewString(), Namespace: ctx.Namespace}
	})

	AfterEach(func() {
		By("Delete VirtualMachine", func() {
			if err := ctx.Client.Delete(ctx, obj); err == nil {
				obj := newObjFn()
				// If VM is still around because of finalizer, try to cleanup for next test.
				if err := ctx.Client.Get(ctx, objKey, obj); err == nil && len(obj.GetFinalizers()) > 0 {
					obj.SetFinalizers(nil)
					_ = ctx.Client.Update(ctx, obj)
				}
			} else {
				Expect(k8serrors.IsNotFound(err)).To(BeTrue())
			}
		})

		obj = nil
		newObjFn = nil
		getInstanceUUIDFn = nil
		pauseAnnotationLabel = ""

		ctx.AfterEach()
		ctx = nil
	})

	getObject := func(
		ctx *builder.IntegrationTestContext,
		objKey types.NamespacedName,
		obj client.Object) client.Object {

		if err := ctx.Client.Get(ctx, objKey, obj); err != nil {
			return nil
		}
		return obj
	}

	waitForVirtualMachineFinalizer := func(
		ctx *builder.IntegrationTestContext,
		objKey types.NamespacedName,
		obj client.Object) {

		Eventually(func() []string {
			if obj := getObject(ctx, objKey, obj); obj != nil {
				return obj.GetFinalizers()
			}
			return nil
		}).Should(ContainElement(finalizer), "waiting for VirtualMachine finalizer")
	}

	reconcile := func() {
		When("the pause annotation is set", func() {
			It("Reconcile returns early and the finalizer never gets added", func() {
				obj.SetAnnotations(map[string]string{
					pauseAnnotationLabel: "",
				})
				Expect(ctx.Client.Create(ctx, obj)).To(Succeed())
				Consistently(func() []string {
					if obj := getObject(ctx, objKey, newObjFn()); obj != nil {
						return obj.GetFinalizers()
					}
					return nil
				}).ShouldNot(ContainElement(finalizer), "waiting for VirtualMachine finalizer")
			})
		})

		It("Reconciles after VirtualMachine creation", func() {
			Expect(ctx.Client.Create(ctx, obj)).To(Succeed())

			By("VirtualMachine should have finalizer added", func() {
				waitForVirtualMachineFinalizer(ctx, objKey, newObjFn())
			})

			By("VirtualMachine should reflect VMProvider updates", func() {
				Eventually(func() string {
					if obj := getObject(ctx, objKey, newObjFn()); obj != nil {
						return getInstanceUUIDFn(obj)
					}
					return ""
				}).ShouldNot(BeEmpty(), "waiting for expected InstanceUUID")
			})

			By("VirtualMachine should not be updated in steady-state", func() {
				obj := getObject(ctx, objKey, newObjFn())
				Expect(obj).ToNot(BeNil())
				rv := obj.GetResourceVersion()
				Expect(rv).ToNot(BeEmpty())
				expected := fmt.Sprintf("%s :: %d", rv, obj.GetGeneration())
				// The resync period is 1 second, so balance between giving enough time vs a slow test.
				// Note: the kube-apiserver we test against (obtained from kubebuilder) is old and
				// appears to behavior differently than newer versions (like used in the SV) in that noop
				// Status subresource updates don't increment the ResourceVersion.
				Consistently(func() string {
					if obj := getObject(ctx, objKey, newObjFn()); obj != nil {
						return fmt.Sprintf("%s :: %d", obj.GetResourceVersion(), obj.GetGeneration())
					}
					return ""
				}, 4*time.Second).Should(Equal(expected))
			})
		})

		When("CreateOrUpdateVirtualMachine returns an error", func() {
			It("VirtualMachine is in Creating Phase", func() {
				Expect(ctx.Client.Create(ctx, obj)).To(Succeed())
				// Wait for initial reconcile.
				waitForVirtualMachineFinalizer(ctx, objKey, newObjFn())
			})
		})

		It("Reconciles after VirtualMachine deletion", func() {
			Expect(ctx.Client.Create(ctx, obj)).To(Succeed())
			// Wait for initial reconcile.
			waitForVirtualMachineFinalizer(ctx, objKey, newObjFn())
			Expect(ctx.Client.Delete(ctx, obj)).To(Succeed())
			By("Finalizer should be removed after deletion", func() {
				Eventually(func() []string {
					if obj := getObject(ctx, objKey, newObjFn()); obj != nil {
						return obj.GetFinalizers()
					}
					return nil
				}).ShouldNot(ContainElement(finalizer))
			})
		})

		When("Provider DeleteVM returns an error", func() {
			It("VirtualMachine is in Deleting Phase", func() {
				Expect(ctx.Client.Create(ctx, obj)).To(Succeed())
				// Wait for initial reconcile.
				waitForVirtualMachineFinalizer(ctx, objKey, newObjFn())
				Expect(ctx.Client.Delete(ctx, obj)).To(Succeed())
				By("Finalizer should still be present", func() {
					obj := getObject(ctx, objKey, newObjFn())
					Expect(obj).ToNot(BeNil())
					Expect(obj.GetFinalizers()).To(ContainElement(finalizer))
				})
			})
		})
	}

	Context("v1alpha2", func() {
		BeforeEach(func() {
			pauseAnnotationLabel = vmopv1a2.PauseAnnotation

			newObjFn = func() client.Object {
				return &vmopv1a2.VirtualMachine{}
			}

			getInstanceUUIDFn = func(obj client.Object) string {
				return obj.(*vmopv1a2.VirtualMachine).Status.InstanceUUID
			}

			obj = &vmopv1a2.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: objKey.Namespace,
					Name:      objKey.Name,
				},
				Spec: vmopv1a2.VirtualMachineSpec{
					ClassName:    ctx.DefaultVMClass,
					ImageName:    ctx.DefaultVMImageForLinux,
					StorageClass: ctx.DefaultStorageClass,
					PowerState:   vmopv1a2.VirtualMachinePowerStateOn,
				},
			}
		})

		Context("Reconcile", reconcile)
	})

	Context("v1alpha1", func() {
		BeforeEach(func() {
			pauseAnnotationLabel = vmopv1.PauseAnnotation

			newObjFn = func() client.Object {
				return &vmopv1.VirtualMachine{}
			}

			getInstanceUUIDFn = func(obj client.Object) string {
				return obj.(*vmopv1.VirtualMachine).Status.InstanceUUID
			}

			obj = &vmopv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: objKey.Namespace,
					Name:      objKey.Name,
				},
				Spec: vmopv1.VirtualMachineSpec{
					ClassName:    ctx.DefaultVMClass,
					ImageName:    ctx.DefaultVMImageForLinux,
					StorageClass: ctx.DefaultStorageClass,
					PowerState:   vmopv1.VirtualMachinePoweredOn,
				},
			}
		})

		Context("Reconcile", reconcile)
	})
}
