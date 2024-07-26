// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package spq_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spqv1 "github.com/vmware-tanzu/vm-operator/external/storage-policy-quota/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	spqutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube/spq"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = DescribeTable("StoragePolicyUsageName",
	func(s string, expected string) {
		Ω(spqutil.StoragePolicyUsageName(s)).Should(Equal(expected))
	},
	Entry("empty input", "", "-vm-usage"),
	Entry("non-empty-input", "my-class", "my-class-vm-usage"),
)

var _ = Describe("IsStorageClassInNamespace", func() {
	var (
		ctx         context.Context
		client      ctrlclient.Client
		withObjects []ctrlclient.Object
		withFuncs   interceptor.Funcs
		namespace   string
		name        string
		ok          bool
		err         error
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContext()
		namespace = "default"
		name = "my-storage-class"
		withFuncs = interceptor.Funcs{}
		withObjects = []ctrlclient.Object{
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			},
			&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			},
		}
	})

	JustBeforeEach(func() {
		client = builder.NewFakeClientWithInterceptors(
			withFuncs, withObjects...)
		ok, err = spqutil.IsStorageClassInNamespace(
			ctx, client, namespace, name)
	})

	When("FSS_PODVMONSTRETCHEDSUPERVISOR is disabled", func() {
		BeforeEach(func() {
			pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
				config.Features.PodVMOnStretchedSupervisor = false
			})
		})
		When("listing resource quotas returns an error", func() {
			BeforeEach(func() {
				withFuncs.List = func(
					ctx context.Context,
					client ctrlclient.WithWatch,
					list ctrlclient.ObjectList,
					opts ...ctrlclient.ListOption) error {

					if _, ok := list.(*corev1.ResourceQuotaList); ok {
						return fmt.Errorf("fake")
					}

					return client.List(ctx, list, opts...)
				}
			})
			It("should return an error", func() {
				Expect(err).To(MatchError("fake"))
				Expect(ok).To(BeFalse())
			})
		})
		When("there is a match", func() {
			BeforeEach(func() {
				withObjects = append(
					withObjects,
					&corev1.ResourceQuota{
						ObjectMeta: metav1.ObjectMeta{
							Namespace:    namespace,
							GenerateName: "fake-",
						},
						Spec: corev1.ResourceQuotaSpec{
							Hard: corev1.ResourceList{
								corev1.ResourceName(name + ".storageclass.storage.k8s.io/fake"): resource.MustParse("10Gi"),
							},
						},
					},
				)
			})
			It("should return true", func() {
				Expect(err).ToNot(HaveOccurred())
				Expect(ok).To(BeTrue())
			})
		})
		When("there is not a match", func() {
			It("should return false", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(spqutil.NotFoundInNamespace{Namespace: namespace, StorageClass: name}))
				Expect(ok).To(BeFalse())
			})
		})
	})

	When("FSS_PODVMONSTRETCHEDSUPERVISOR is enabled", func() {
		BeforeEach(func() {
			pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
				config.Features.PodVMOnStretchedSupervisor = true
			})
		})
		When("listing storage classes returns an error", func() {
			BeforeEach(func() {
				withFuncs.List = func(
					ctx context.Context,
					client ctrlclient.WithWatch,
					list ctrlclient.ObjectList,
					opts ...ctrlclient.ListOption) error {

					if _, ok := list.(*spqv1.StoragePolicyQuotaList); ok {
						return fmt.Errorf("fake")
					}

					return client.List(ctx, list, opts...)
				}
			})
			It("should return an error", func() {
				Expect(err).To(MatchError("fake"))
				Expect(ok).To(BeFalse())
			})
		})
		When("there is a match", func() {
			BeforeEach(func() {
				withObjects = append(
					withObjects,
					&spqv1.StoragePolicyQuota{
						ObjectMeta: metav1.ObjectMeta{
							Namespace:    namespace,
							GenerateName: "fake-",
						},
						Status: spqv1.StoragePolicyQuotaStatus{
							SCLevelQuotaStatuses: spqv1.SCLevelQuotaStatusList{
								{
									StorageClassName: name,
								},
							},
						},
					},
				)
			})
			It("should return true", func() {
				Expect(err).ToNot(HaveOccurred())
				Expect(ok).To(BeTrue())
			})
		})
		When("there is not a match", func() {
			It("should return false", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(spqutil.NotFoundInNamespace{Namespace: namespace, StorageClass: name}))
				Expect(ok).To(BeFalse())
			})
		})
	})
})

var _ = Describe("GetStorageClassInNamespace", func() {
	var (
		ctx         context.Context
		client      ctrlclient.Client
		withObjects []ctrlclient.Object
		withFuncs   interceptor.Funcs
		namespace   string
		name        string
		inName      string
		obj         storagev1.StorageClass
		err         error
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContext()
		namespace = "default"
		name = "my-storage-class"
		inName = name
		withFuncs = interceptor.Funcs{}
		withObjects = []ctrlclient.Object{
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			},
			&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			},
		}
	})

	JustBeforeEach(func() {
		client = builder.NewFakeClientWithInterceptors(
			withFuncs, withObjects...)

		obj, err = spqutil.GetStorageClassInNamespace(
			ctx, client, namespace, inName)
	})

	When("storage class does not exist", func() {
		BeforeEach(func() {
			inName = "fake"
		})
		It("should return NotFound", func() {
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(obj).To(BeZero())
		})
	})

	When("storage class does exist", func() {
		When("FSS_PODVMONSTRETCHEDSUPERVISOR is enabled", func() {
			BeforeEach(func() {
				pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
					config.Features.PodVMOnStretchedSupervisor = true
				})
			})
			When("listing storage policy quotas returns an error", func() {
				BeforeEach(func() {
					withFuncs.List = func(
						ctx context.Context,
						client ctrlclient.WithWatch,
						list ctrlclient.ObjectList,
						opts ...ctrlclient.ListOption) error {

						if _, ok := list.(*spqv1.StoragePolicyQuotaList); ok {
							return fmt.Errorf("fake")
						}

						return client.List(ctx, list, opts...)
					}
				})
				It("should return an error", func() {
					Expect(err).To(MatchError("fake"))
					Expect(obj).To(BeZero())
				})
			})
			When("there is a match", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace,
								GenerateName: "fake-",
							},
							Status: spqv1.StoragePolicyQuotaStatus{
								SCLevelQuotaStatuses: spqv1.SCLevelQuotaStatusList{
									{
										StorageClassName: name,
									},
								},
							},
						},
					)
				})
				It("should return the storage class", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(obj.Name).To(Equal(name))
				})
			})
			When("there is not a match", func() {
				It("should return NotFoundInNamespace", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(MatchError(spqutil.NotFoundInNamespace{Namespace: namespace, StorageClass: name}))
					Expect(obj).To(BeZero())
				})
			})
		})
	})
})

var _ = Describe("NotFoundInNamespace", func() {
	err := spqutil.NotFoundInNamespace{
		Namespace:    "fake",
		StorageClass: "my-storage-class",
	}
	Context("Error", func() {
		It("should return the expected string", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("Storage policy is not associated with the namespace fake"))
		})
	})
	Context("String", func() {
		It("should return the expected string", func() {
			Expect(err).To(HaveOccurred())
			Expect(err.String()).To(Equal("Storage policy is not associated with the namespace fake"))
		})
	})
})

var _ = Describe("GetStoragePolicyIDFromClass", func() {
	var (
		ctx         context.Context
		client      ctrlclient.Client
		withObjects []ctrlclient.Object
		namespace   string
		name        string
		inName      string
		id          string
		err         error
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContext()
		namespace = "default"
		name = "my-storage-class"
		inName = name
		withObjects = []ctrlclient.Object{
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			},
			&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Parameters: map[string]string{
					"storagePolicyID": "my-policy-id",
				},
			},
		}
	})

	JustBeforeEach(func() {
		client = builder.NewFakeClient(withObjects...)

		id, err = spqutil.GetStoragePolicyIDFromClass(
			ctx, client, inName)
	})

	When("storage class does not exist", func() {
		BeforeEach(func() {
			inName = "fake"
		})
		It("should return NotFound", func() {
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(id).To(BeEmpty())
		})
	})

	When("storage class does exist", func() {
		It("should return the expected id", func() {
			Expect(err).ToNot(HaveOccurred())
			Expect(id).To(Equal("my-policy-id"))
		})
	})
})

var _ = Describe("GetStorageClassesForPolicy", func() {
	var (
		ctx         context.Context
		client      ctrlclient.Client
		withObjects []ctrlclient.Object
		withFuncs   interceptor.Funcs
		namespace   string
		policyID    string
		inPolicyID  string
		sc1Name     string
		sc1PolicyID string
		sc2Name     string
		sc2PolicyID string
		obj         []storagev1.StorageClass
		err         error
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContext()
		namespace = "default"
		policyID = "my-policy-id"
		inPolicyID = policyID
		sc1Name = "my-storage-class-1"
		sc1PolicyID = policyID
		sc2Name = "my-storage-class-2"
		sc2PolicyID = policyID
		withFuncs = interceptor.Funcs{}
		withObjects = []ctrlclient.Object{
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace,
				},
			},
		}
	})

	JustBeforeEach(func() {
		withObjects = append(
			withObjects,
			&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: sc1Name,
				},
				Parameters: map[string]string{
					"storagePolicyID": sc1PolicyID,
				},
			},
			&storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: sc2Name,
				},
				Parameters: map[string]string{
					"storagePolicyID": sc2PolicyID,
				},
			},
		)

		client = builder.NewFakeClientWithInterceptors(
			withFuncs, withObjects...)

		obj, err = spqutil.GetStorageClassesForPolicy(
			ctx, client, namespace, inPolicyID)
	})

	When("policy id does not exist", func() {
		BeforeEach(func() {
			inPolicyID = "fake"
		})
		It("should return an empty list", func() {
			Expect(err).ToNot(HaveOccurred())
			Expect(obj).To(BeEmpty())
		})
	})

	When("storage classes are available", func() {
		When("FSS_PODVMONSTRETCHEDSUPERVISOR is enabled", func() {
			BeforeEach(func() {
				pkgcfg.SetContext(ctx, func(config *pkgcfg.Config) {
					config.Features.PodVMOnStretchedSupervisor = true
				})
			})
			When("listing storage classes returns an error", func() {
				BeforeEach(func() {
					withFuncs.List = func(
						ctx context.Context,
						client ctrlclient.WithWatch,
						list ctrlclient.ObjectList,
						opts ...ctrlclient.ListOption) error {

						if _, ok := list.(*storagev1.StorageClassList); ok {
							return fmt.Errorf("fake")
						}

						return client.List(ctx, list, opts...)
					}
				})
				It("should return an error", func() {
					Expect(err).To(MatchError("fake"))
					Expect(obj).To(BeZero())
				})
			})
			When("listing storage policy quotas returns an error", func() {
				BeforeEach(func() {
					withFuncs.List = func(
						ctx context.Context,
						client ctrlclient.WithWatch,
						list ctrlclient.ObjectList,
						opts ...ctrlclient.ListOption) error {

						if _, ok := list.(*spqv1.StoragePolicyQuotaList); ok {
							return fmt.Errorf("fake")
						}

						return client.List(ctx, list, opts...)
					}
				})
				It("should return an error", func() {
					Expect(err).To(MatchError("fake"))
					Expect(obj).To(BeZero())
				})
			})
			When("there is a match", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace,
								GenerateName: "fake-",
							},
							Status: spqv1.StoragePolicyQuotaStatus{
								SCLevelQuotaStatuses: spqv1.SCLevelQuotaStatusList{
									{
										StorageClassName: sc1Name,
									},
								},
							},
						},
					)
				})
				It("should return the storage class", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(obj).To(HaveLen(1))
					Expect(obj[0].Name).To(Equal(sc1Name))
				})
			})
			When("there are multiple matches", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace,
								GenerateName: "fake-",
							},
							Status: spqv1.StoragePolicyQuotaStatus{
								SCLevelQuotaStatuses: spqv1.SCLevelQuotaStatusList{
									{
										StorageClassName: sc1Name,
									},
									{
										StorageClassName: sc2Name,
									},
								},
							},
						},
					)
				})
				It("should return the storage classes", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(obj).To(HaveLen(2))
					Expect(obj[0].Name).To(Equal(sc1Name))
					Expect(obj[1].Name).To(Equal(sc2Name))
				})
			})
			When("there is not a match", func() {
				It("should return an empty list", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(obj).To(BeEmpty())
				})
			})
		})
	})
})
