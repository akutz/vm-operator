// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package vmopv1_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	vimtypes "github.com/vmware/govmomi/vim25/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha3"
	spqv1 "github.com/vmware-tanzu/vm-operator/external/storage-policy-quota/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	spqutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube/spq"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe("ErrImageNotFound", func() {
	It("should return true from apierrors.IsNotFound", func() {
		Expect(apierrors.IsNotFound(vmopv1util.ErrImageNotFound{})).To(BeTrue())
	})
})

var _ = Describe("ResolveImageName", func() {

	const (
		actualNamespace = "my-namespace"

		nsImg1ID   = "vmi-1"
		nsImg1Name = "image-a"

		nsImg2ID   = "vmi-2"
		nsImg2Name = "image-b"

		nsImg3ID   = "vmi-3"
		nsImg3Name = "image-b"

		nsImg4ID   = "vmi-4"
		nsImg4Name = "image-c"

		clImg1ID   = "vmi-5"
		clImg1Name = "image-d"

		clImg2ID   = "vmi-6"
		clImg2Name = "image-e"

		clImg3ID   = "vmi-7"
		clImg3Name = "image-e"

		clImg4ID   = "vmi-8"
		clImg4Name = "image-c"
	)

	var (
		name      string
		namespace string
		client    ctrlclient.Client
		err       error
		obj       ctrlclient.Object
	)

	BeforeEach(func() {
		namespace = actualNamespace

		newNsImgFn := func(id, name string) *vmopv1.VirtualMachineImage {
			img := builder.DummyVirtualMachineImage(id)
			img.Namespace = actualNamespace
			img.Status.Name = name
			return img
		}

		newClImgFn := func(id, name string) *vmopv1.ClusterVirtualMachineImage {
			img := builder.DummyClusterVirtualMachineImage(id)
			img.Status.Name = name
			return img
		}

		// Replace the client with a fake client that has the index of VM images.
		client = fake.NewClientBuilder().WithScheme(builder.NewScheme()).
			WithIndex(
				&vmopv1.VirtualMachineImage{},
				"status.name",
				func(rawObj ctrlclient.Object) []string {
					image := rawObj.(*vmopv1.VirtualMachineImage)
					return []string{image.Status.Name}
				}).
			WithIndex(&vmopv1.ClusterVirtualMachineImage{},
				"status.name",
				func(rawObj ctrlclient.Object) []string {
					image := rawObj.(*vmopv1.ClusterVirtualMachineImage)
					return []string{image.Status.Name}
				}).
			WithObjects(
				newNsImgFn(nsImg1ID, nsImg1Name),
				newNsImgFn(nsImg2ID, nsImg2Name),
				newNsImgFn(nsImg3ID, nsImg3Name),
				newNsImgFn(nsImg4ID, nsImg4Name),
				newClImgFn(clImg1ID, clImg1Name),
				newClImgFn(clImg2ID, clImg2Name),
				newClImgFn(clImg3ID, clImg3Name),
				newClImgFn(clImg4ID, clImg4Name),
			).
			Build()
	})

	JustBeforeEach(func() {
		obj, err = vmopv1util.ResolveImageName(
			context.Background(), client, namespace, name)
	})

	When("name is vmi", func() {
		When("no image exists", func() {
			const missingVmi = "vmi-9999999"
			BeforeEach(func() {
				name = missingVmi
			})
			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				Expect(err).To(BeAssignableToTypeOf(vmopv1util.ErrImageNotFound{}))
				Expect(err.Error()).To(Equal(fmt.Sprintf("no VM image exists for %q in namespace or cluster scope", missingVmi)))
				Expect(obj).To(BeNil())
			})
		})
		When("img is namespace-scoped", func() {
			BeforeEach(func() {
				name = nsImg1ID
			})
			It("should return image ref", func() {
				Expect(err).ToNot(HaveOccurred())
				Expect(obj).To(BeAssignableToTypeOf(&vmopv1.VirtualMachineImage{}))
				img := obj.(*vmopv1.VirtualMachineImage)
				Expect(img.Name).To(Equal(nsImg1ID))
			})
		})
		When("img is cluster-scoped", func() {
			BeforeEach(func() {
				name = clImg1ID
			})
			It("should return image ref", func() {
				Expect(err).ToNot(HaveOccurred())
				Expect(obj).To(BeAssignableToTypeOf(&vmopv1.ClusterVirtualMachineImage{}))
				img := obj.(*vmopv1.ClusterVirtualMachineImage)
				Expect(img.Name).To(Equal(clImg1ID))
			})
		})
	})

	When("name is display name", func() {
		BeforeEach(func() {
			name = nsImg1Name
		})
		It("should return image ref", func() {
			Expect(err).ToNot(HaveOccurred())
			Expect(obj).To(BeAssignableToTypeOf(&vmopv1.VirtualMachineImage{}))
			img := obj.(*vmopv1.VirtualMachineImage)
			Expect(img.Name).To(Equal(nsImg1ID))
		})
	})
	When("name is empty", func() {
		BeforeEach(func() {
			name = ""
		})
		It("should return an error", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("imgName is empty"))
			Expect(obj).To(BeNil())
		})
	})

	When("name matches multiple, namespaced-scoped images", func() {
		BeforeEach(func() {
			name = nsImg2Name
		})
		It("should return an error", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(fmt.Sprintf("multiple VM images exist for %q in namespace scope", nsImg2Name)))
			Expect(obj).To(BeNil())
		})
	})

	When("name matches multiple, cluster-scoped images", func() {
		BeforeEach(func() {
			name = clImg2Name
		})
		It("should return an error", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(fmt.Sprintf("multiple VM images exist for %q in cluster scope", clImg2Name)))
			Expect(obj).To(BeNil())
		})
	})

	When("name matches both namespace and cluster-scoped images", func() {
		BeforeEach(func() {
			name = clImg4Name
		})
		It("should return an error", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(fmt.Sprintf("multiple VM images exist for %q in namespace and cluster scope", clImg4Name)))
			Expect(obj).To(BeNil())
		})
	})

	When("name does not match any namespace or cluster-scoped images", func() {
		const invalidImageID = "invalid"
		BeforeEach(func() {
			name = invalidImageID
		})
		It("should return an error", func() {
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(err).To(BeAssignableToTypeOf(vmopv1util.ErrImageNotFound{}))
			Expect(err.Error()).To(Equal(fmt.Sprintf("no VM image exists for %q in namespace or cluster scope", invalidImageID)))
			Expect(obj).To(BeNil())
		})
	})

	When("name matches a single namespace-scoped image", func() {
		BeforeEach(func() {
			name = nsImg1Name
		})
		It("should return image ref", func() {
			Expect(err).ToNot(HaveOccurred())
			Expect(obj).To(BeAssignableToTypeOf(&vmopv1.VirtualMachineImage{}))
			img := obj.(*vmopv1.VirtualMachineImage)
			Expect(img.Name).To(Equal(nsImg1ID))
		})
	})

	When("name matches a single cluster-scoped image", func() {
		BeforeEach(func() {
			name = clImg1Name
		})
		It("should return image ref", func() {
			Expect(err).ToNot(HaveOccurred())
			Expect(obj).To(BeAssignableToTypeOf(&vmopv1.ClusterVirtualMachineImage{}))
			img := obj.(*vmopv1.ClusterVirtualMachineImage)
			Expect(img.Name).To(Equal(clImg1ID))
		})
	})
})

var _ = DescribeTable("DetermineHardwareVersion",
	func(
		vm vmopv1.VirtualMachine,
		configSpec vimtypes.VirtualMachineConfigSpec,
		imgStatus vmopv1.VirtualMachineImageStatus,
		expected vimtypes.HardwareVersion,
	) {
		Ω(vmopv1util.DetermineHardwareVersion(vm, configSpec, imgStatus)).Should(Equal(expected))
	},
	Entry(
		"empty inputs",
		vmopv1.VirtualMachine{},
		vimtypes.VirtualMachineConfigSpec{},
		vmopv1.VirtualMachineImageStatus{},
		vimtypes.HardwareVersion(0),
	),
	Entry(
		"spec.minHardwareVersion is 11",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
			},
		},
		vimtypes.VirtualMachineConfigSpec{},
		vmopv1.VirtualMachineImageStatus{},
		vimtypes.HardwareVersion(11),
	),
	Entry(
		"spec.minHardwareVersion is 11, configSpec.version is 13",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
			},
		},
		vimtypes.VirtualMachineConfigSpec{
			Version: "vmx-13",
		},
		vmopv1.VirtualMachineImageStatus{},
		vimtypes.HardwareVersion(13),
	),
	Entry(
		"spec.minHardwareVersion is 11, configSpec.version is invalid",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
			},
		},
		vimtypes.VirtualMachineConfigSpec{
			Version: "invalid",
		},
		vmopv1.VirtualMachineImageStatus{},
		vimtypes.HardwareVersion(11),
	),
	Entry(
		"spec.minHardwareVersion is 11, configSpec has pci pass-through",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
			},
		},
		vimtypes.VirtualMachineConfigSpec{
			DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
				&vimtypes.VirtualDeviceConfigSpec{
					Device: &vimtypes.VirtualPCIPassthrough{},
				},
			},
		},
		vmopv1.VirtualMachineImageStatus{},
		pkgconst.MinSupportedHWVersionForPCIPassthruDevices,
	),
	Entry(
		"spec.minHardwareVersion is 11, vm has pvc",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
				Volumes: []vmopv1.VirtualMachineVolume{
					{
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{},
						},
					},
				},
			},
		},
		vimtypes.VirtualMachineConfigSpec{},
		vmopv1.VirtualMachineImageStatus{},
		pkgconst.MinSupportedHWVersionForPVC,
	),
	Entry(
		"spec.minHardwareVersion is 11, configSpec has pci pass-through, image version is 20",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
			},
		},
		vimtypes.VirtualMachineConfigSpec{
			DeviceChange: []vimtypes.BaseVirtualDeviceConfigSpec{
				&vimtypes.VirtualDeviceConfigSpec{
					Device: &vimtypes.VirtualPCIPassthrough{},
				},
			},
		},
		vmopv1.VirtualMachineImageStatus{
			HardwareVersion: &[]int32{20}[0],
		},
		vimtypes.HardwareVersion(20),
	),
	Entry(
		"spec.minHardwareVersion is 11, vm has pvc, image version is 20",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				MinHardwareVersion: 11,
				Volumes: []vmopv1.VirtualMachineVolume{
					{
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{},
						},
					},
				},
			},
		},
		vimtypes.VirtualMachineConfigSpec{},
		vmopv1.VirtualMachineImageStatus{
			HardwareVersion: &[]int32{20}[0],
		},
		vimtypes.HardwareVersion(20),
	),
)

var _ = DescribeTable("HasPVC",
	func(
		vm vmopv1.VirtualMachine,
		expected bool,
	) {
		Ω(vmopv1util.HasPVC(vm)).Should(Equal(expected))
	},
	Entry(
		"spec.volumes is empty",
		vmopv1.VirtualMachine{},
		false,
	),
	Entry(
		"spec.volumes is non-empty with no pvcs",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Volumes: []vmopv1.VirtualMachineVolume{
					{
						Name: "hello",
					},
				},
			},
		},
		false,
	),
	Entry(
		"spec.volumes is non-empty with at least one pvc",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Volumes: []vmopv1.VirtualMachineVolume{
					{
						Name: "hello",
					},
					{
						Name: "world",
						VirtualMachineVolumeSource: vmopv1.VirtualMachineVolumeSource{
							PersistentVolumeClaim: &vmopv1.PersistentVolumeClaimVolumeSource{},
						},
					},
				},
			},
		},
		true,
	),
)

var _ = DescribeTable("IsClasslessVM",
	func(
		vm vmopv1.VirtualMachine,
		expected bool,
	) {
		Ω(vmopv1util.IsClasslessVM(vm)).Should(Equal(expected))
	},
	Entry(
		"spec.className is empty",
		vmopv1.VirtualMachine{},
		true,
	),
	Entry(
		"spec.className is non-empty",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				ClassName: "small",
			},
		},
		false,
	),
)

var _ = DescribeTable("IsImageLessVM",
	func(
		vm vmopv1.VirtualMachine,
		expected bool,
	) {
		Ω(vmopv1util.IsImagelessVM(vm)).Should(Equal(expected))
	},
	Entry(
		"spec.image is nil and spec.imageName is empty",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Image:     nil,
				ImageName: "",
			},
		},
		true,
	),
	Entry(
		"spec.image is not nil and spec.imageName is empty",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Image:     &vmopv1.VirtualMachineImageRef{},
				ImageName: "",
			},
		},
		false,
	),
	Entry(
		"spec.image is nil and spec.imageName is non-empty",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Image:     nil,
				ImageName: "non-empty",
			},
		},
		false,
	),
	Entry(
		"spec.image is not nil and spec.imageName is non-empty",
		vmopv1.VirtualMachine{
			Spec: vmopv1.VirtualMachineSpec{
				Image:     &vmopv1.VirtualMachineImageRef{},
				ImageName: "non-empty",
			},
		},
		false,
	),
)

var _ = Describe("SyncStorageUsageForNamespace", func() {

	var (
		ctx                       context.Context
		namespace                 string
		storageClass              string
		client                    ctrlclient.Client
		withFuncs                 interceptor.Funcs
		withObjects               []ctrlclient.Object
		isScheduled               bool
		err                       error
		skipGetStoragePolicyUsage bool
	)

	BeforeEach(func() {
		ctx = spqutil.WithContext(pkgcfg.NewContext())
		withObjects = nil
		withFuncs = interceptor.Funcs{}
		namespace = "default"
		storageClass = "my-storage-class"
		skipGetStoragePolicyUsage = false
	})

	JustBeforeEach(func() {
		client = builder.NewFakeClientWithInterceptors(
			withFuncs, withObjects...)

		var chanErr <-chan error
		isScheduled, chanErr = vmopv1util.SyncStorageUsageForNamespace(
			ctx,
			client,
			namespace,
			storageClass)

		err = <-chanErr
	})

	When("there is no StoragePolicyUsage resource", func() {
		It("should no-op", func() {
			Expect(isScheduled).To(BeTrue())
			Expect(err).To(MatchError(
				fmt.Sprintf(
					"failed to get StoragePolicyUsage %[1]s/%[2]s-vm-usage: "+
						"storagepolicyusages.cns.vmware.com \"%[2]s-vm-usage\" not found",
					namespace, storageClass)))
		})
	})

	When("there is a StoragePolicyUsage resource", func() {
		var (
			spu spqv1.StoragePolicyUsage
		)
		BeforeEach(func() {
			withObjects = append(
				withObjects,
				&spqv1.StoragePolicyUsage{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      spqutil.StoragePolicyUsageName(storageClass),
					},
					Spec: spqv1.StoragePolicyUsageSpec{
						StoragePolicyId:  "fake",
						StorageClassName: storageClass,
					},
				})
		})
		AfterEach(func() {
			spu = spqv1.StoragePolicyUsage{}
		})
		JustBeforeEach(func() {
			if !skipGetStoragePolicyUsage {
				Expect(client.Get(
					ctx,
					ctrlclient.ObjectKey{
						Namespace: namespace,
						Name:      spqutil.StoragePolicyUsageName(storageClass),
					},
					&spu,
				)).To(Succeed())
			}
		})
		When("there is an error getting the StoragePolicyUsage resource the first time", func() {
			BeforeEach(func() {
				skipGetStoragePolicyUsage = true
				numCalls := 0
				withFuncs.Get = func(
					ctx context.Context,
					client ctrlclient.WithWatch,
					key ctrlclient.ObjectKey,
					obj ctrlclient.Object,
					opts ...ctrlclient.GetOption) error {

					if _, ok := obj.(*spqv1.StoragePolicyUsage); ok {
						if numCalls == 0 {
							return fmt.Errorf("fake")
						}
						numCalls++
					}

					return client.Get(ctx, key, obj, opts...)
				}
			})
			It("should return an error", func() {
				Expect(isScheduled).To(BeTrue())
				Expect(err).To(MatchError(
					fmt.Sprintf(
						"failed to get StoragePolicyUsage %s/%s-vm-usage: %s",
						namespace, storageClass, "fake")))
			})
		})
		When("there is an error listing VM resources", func() {
			BeforeEach(func() {
				withFuncs.List = func(
					ctx context.Context,
					client ctrlclient.WithWatch,
					list ctrlclient.ObjectList,
					opts ...ctrlclient.ListOption) error {

					if _, ok := list.(*vmopv1.VirtualMachineList); ok {
						return fmt.Errorf("fake")
					}

					return client.List(ctx, list, opts...)
				}
			})
			It("should return an error", func() {
				Expect(isScheduled).To(BeTrue())
				Expect(err).To(MatchError(
					fmt.Sprintf(
						"failed to list VMs in namespace %s: %s",
						namespace, "fake")))
			})
		})
		When("there are no VM resources", func() {
			Specify("the usage should remain empty", func() {
				Expect(isScheduled).To(BeTrue())
				Expect(err).ToNot(HaveOccurred())
				Eventually(spu.Status.ResourceTypeLevelQuotaUsage).ShouldNot(BeNil())
				Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).ShouldNot(BeNil())
				Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved.Value()).Should(Equal(int64(0)))
				Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).ShouldNot(BeNil())
				Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used.Value()).Should(Equal(int64(0)))
			})
		})
		When("there are VM resources", func() {
			When("there is an error getting the StoragePolicyUsage resource the second time", func() {
				BeforeEach(func() {
					skipGetStoragePolicyUsage = true
					numCalls := 0
					withFuncs.Get = func(
						ctx context.Context,
						client ctrlclient.WithWatch,
						key ctrlclient.ObjectKey,
						obj ctrlclient.Object,
						opts ...ctrlclient.GetOption) error {

						if _, ok := obj.(*spqv1.StoragePolicyUsage); ok {
							if numCalls == 1 {
								return fmt.Errorf("fake")
							}
							numCalls++
						}

						return client.Get(ctx, key, obj, opts...)
					}
				})
				It("should return an error", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).To(MatchError(
						fmt.Sprintf(
							"failed to get StoragePolicyUsage %s/%s-vm-usage: %s",
							namespace, storageClass, "fake")))
				})
			})
			When("there is an error patching the StoragePolicyUsage resource", func() {
				BeforeEach(func() {
					withFuncs.SubResourcePatch = func(
						ctx context.Context,
						client ctrlclient.Client,
						subResourceName string,
						obj ctrlclient.Object,
						patch ctrlclient.Patch,
						opts ...ctrlclient.SubResourcePatchOption) error {

						if _, ok := obj.(*spqv1.StoragePolicyUsage); ok {
							return fmt.Errorf("fake")
						}

						return client.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
					}
				})
				It("should return an error", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).To(MatchError(
						fmt.Sprintf(
							"failed to patch StoragePolicyUsage %s/%s-vm-usage: %s",
							namespace, storageClass, "fake")))
				})
			})
			When("two sync requests happen concurrently", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-1",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("10Gi")),
									Uncommitted: ptr.To(resource.MustParse("20Gi")),
									Unshared:    ptr.To(resource.MustParse("5Gi")),
								},
								Volumes: []vmopv1.VirtualMachineVolumeStatus{
									{
										Name:  "vm-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeClassic,
										Limit: ptr.To(resource.MustParse("20Gi")),
										Used:  ptr.To(resource.MustParse("10Gi")),
									},
								},
							},
						},
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-2",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("5Gi")),
									Uncommitted: ptr.To(resource.MustParse("10Gi")),
									Unshared:    ptr.To(resource.MustParse("2Gi")),
								},
							},
						},
					)
				})
				Specify("only one request should be executed", func() {

					var (
						wait         = make(chan struct{})
						ready        sync.WaitGroup
						done         sync.WaitGroup
						isScheduled1 bool
						isScheduled2 bool
					)

					ready.Add(2)
					done.Add(2)

					fn := func(isScheduled *bool) {
						ready.Done()
						<-wait
						*isScheduled, _ = vmopv1util.SyncStorageUsageForNamespace(
							ctx, client, namespace, storageClass)
						done.Done()
					}

					go fn(&isScheduled1)
					go fn(&isScheduled2)

					ready.Wait()
					close(wait)
					done.Wait()

					numScheduled := 0
					if isScheduled1 {
						numScheduled++
					}
					if isScheduled2 {
						numScheduled++
					}

					Expect(numScheduled).Should(Equal(1))
				})
			})
			Context("that are being deleted", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:         namespace,
								Name:              "vm-1",
								DeletionTimestamp: &metav1.Time{Time: time.Now()},
								Finalizers:        []string{"fake.com/finalizer"},
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("10Gi")),
									Uncommitted: ptr.To(resource.MustParse("20Gi")),
									Unshared:    ptr.To(resource.MustParse("5Gi")),
								},
								Volumes: []vmopv1.VirtualMachineVolumeStatus{
									{
										Name:  "vm-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeClassic,
										Limit: ptr.To(resource.MustParse("20Gi")),
										Used:  ptr.To(resource.MustParse("10Gi")),
									},
								},
							},
						},
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-2",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("5Gi")),
									Uncommitted: ptr.To(resource.MustParse("10Gi")),
									Unshared:    ptr.To(resource.MustParse("2Gi")),
								},
							},
						},
					)
				})
				Specify("the used/reserved information should be reported for non-deleted VMs", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).ToNot(HaveOccurred())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).Should(Equal(ptr.To(resource.MustParse("15Gi"))))
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).Should(Equal(ptr.To(resource.MustParse("2Gi"))))
				})
			})
			Context("that have no storage status", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-1",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("10Gi")),
									Uncommitted: ptr.To(resource.MustParse("20Gi")),
									Unshared:    ptr.To(resource.MustParse("5Gi")),
								},
								Volumes: []vmopv1.VirtualMachineVolumeStatus{
									{
										Name:  "vm-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeClassic,
										Limit: ptr.To(resource.MustParse("20Gi")),
										Used:  ptr.To(resource.MustParse("10Gi")),
									},
								},
							},
						},
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-2",
							},
							Status: vmopv1.VirtualMachineStatus{},
						},
					)
				})
				Specify("the reported used/reserved information should only include known data", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).ToNot(HaveOccurred())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).Should(Equal(ptr.To(resource.MustParse("30Gi"))))
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).Should(Equal(ptr.To(resource.MustParse("5Gi"))))
				})
			})
			Context("with no PVCs", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-1",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("10Gi")),
									Uncommitted: ptr.To(resource.MustParse("20Gi")),
									Unshared:    ptr.To(resource.MustParse("5Gi")),
								},
								Volumes: []vmopv1.VirtualMachineVolumeStatus{
									{
										Name:  "vm-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeClassic,
										Limit: ptr.To(resource.MustParse("20Gi")),
										Used:  ptr.To(resource.MustParse("10Gi")),
									},
								},
							},
						},
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-2",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("5Gi")),
									Uncommitted: ptr.To(resource.MustParse("10Gi")),
									Unshared:    ptr.To(resource.MustParse("2Gi")),
								},
							},
						},
					)
				})
				Specify("the reported used/reserved information should subtract any PVC data", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).ToNot(HaveOccurred())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).Should(Equal(ptr.To(resource.MustParse("45Gi"))))
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).Should(Equal(ptr.To(resource.MustParse("7Gi"))))
				})
			})
			Context("with PVCs", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-1",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("10Gi")),
									Uncommitted: ptr.To(resource.MustParse("20Gi")),
									Unshared:    ptr.To(resource.MustParse("5Gi")),
								},
								Volumes: []vmopv1.VirtualMachineVolumeStatus{
									{
										Name:  "vm-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeClassic,
										Limit: ptr.To(resource.MustParse("10Gi")),
										Used:  ptr.To(resource.MustParse("5Gi")),
									},
									{
										Name:  "vol-1",
										Type:  vmopv1.VirtualMachineStorageDiskTypeManaged,
										Limit: ptr.To(resource.MustParse("10Gi")),
										Used:  ptr.To(resource.MustParse("5Gi")),
									},
								},
							},
						},
						&vmopv1.VirtualMachine{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: namespace,
								Name:      "vm-2",
							},
							Status: vmopv1.VirtualMachineStatus{
								Storage: &vmopv1.VirtualMachineStorageStatus{
									Committed:   ptr.To(resource.MustParse("5Gi")),
									Uncommitted: ptr.To(resource.MustParse("10Gi")),
									Unshared:    ptr.To(resource.MustParse("2Gi")),
								},
							},
						},
					)
				})
				Specify("the reported used/reserved information should subtract any PVC data", func() {
					Expect(isScheduled).To(BeTrue())
					Expect(err).ToNot(HaveOccurred())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Reserved).Should(Equal(ptr.To(resource.MustParse("35Gi"))))
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).ShouldNot(BeNil())
					Eventually(spu.Status.ResourceTypeLevelQuotaUsage.Used).Should(Equal(ptr.To(resource.MustParse("2Gi"))))
				})
			})
		})
	})
})
