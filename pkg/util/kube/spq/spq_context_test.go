// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package spq_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spqv1 "github.com/vmware-tanzu/vm-operator/external/storage-policy-quota/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	spqutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube/spq"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var noopExecuteFn = func() {}

var _ = Describe("JoinContext", func() {
	When("parent is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.JoinContext(nil, spqutil.NewContext())
			}
			Expect(fn).To(PanicWith("parent context is nil"))
		})
	})
	When("contextWithMap is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.JoinContext(context.Background(), nil)
			}
			Expect(fn).To(PanicWith("contextWithMap is nil"))
		})
	})
	When("map is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.JoinContext(context.Background(), context.Background())
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})
	When("the args are correct", func() {
		It("should return a new context", func() {
			ctx := spqutil.JoinContext(context.Background(), spqutil.NewContext())
			Expect(ctx).ToNot(BeNil())
		})
	})
})

var _ = Describe("WithContext", func() {
	When("parent is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.WithContext(nil)
			}
			Expect(fn).To(PanicWith("parent context is nil"))
		})
	})
	When("parent is not nil", func() {
		It("should return a context", func() {
			ctx := spqutil.WithContext(context.Background())
			Expect(ctx).ToNot(BeNil())
		})
	})
})

var _ = Describe("DeleteChanForStoragePolicy", func() {
	var (
		ctx         context.Context
		inCtx       context.Context
		client      ctrlclient.Client
		withObjects []ctrlclient.Object
		withFuncs   interceptor.Funcs
		namespace1  string
		namespace2  string
		policyID    string
		inPolicyID  string
		sc1Name     string
		sc1PolicyID string
		sc2Name     string
		sc2PolicyID string
		err         error
	)

	BeforeEach(func() {
		ctx = spqutil.WithContext(pkgcfg.NewContext())
		inCtx = ctx
		namespace1 = "my-ns-1"
		namespace2 = "my-ns-2"
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
					Name: namespace1,
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

		// Create the channels.
		_ = spqutil.ExecuteForStorageClass(ctx, namespace1, sc1Name, noopExecuteFn)
		_ = spqutil.ExecuteForStorageClass(ctx, namespace1, sc2Name, noopExecuteFn)
		_ = spqutil.ExecuteForStorageClass(ctx, namespace2, sc2Name, noopExecuteFn)

		// Assert the channels exist.
		Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc1Name)).To(BeTrue())
		Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc2Name)).To(BeTrue())
		Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace2, sc2Name)).To(BeTrue())

		err = spqutil.DeleteChanForStoragePolicy(
			inCtx, client, namespace1, inPolicyID)
	})

	When("ctx is nil", func() {
		BeforeEach(func() {
			inCtx = nil
		})
		It("should no-op and not return an error", func() {
			Expect(err).ToNot(HaveOccurred())
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
				})
			})
			When("a single storage class exists for that policy ID and is in the namespace", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace1,
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
				It("should delete the channels for the storage classes in the namespace", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc1Name)).To(BeFalse())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc2Name)).To(BeTrue())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace2, sc2Name)).To(BeTrue())
				})
			})
			When("multiple storage classes exists for that policy ID but only one is in the namespace", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace1,
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
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace2,
								GenerateName: "fake-",
							},
							Status: spqv1.StoragePolicyQuotaStatus{
								SCLevelQuotaStatuses: spqv1.SCLevelQuotaStatusList{
									{
										StorageClassName: sc2Name,
									},
								},
							},
						},
					)
				})
				It("should delete the channels for the storage classes in the namespace", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc1Name)).To(BeFalse())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc2Name)).To(BeTrue())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace2, sc2Name)).To(BeTrue())
				})
			})
			When("multiple storage classes exists for that policy ID and are in the namespace", func() {
				BeforeEach(func() {
					withObjects = append(
						withObjects,
						&spqv1.StoragePolicyQuota{
							ObjectMeta: metav1.ObjectMeta{
								Namespace:    namespace1,
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
				It("should delete the channels for the storage classes in the namespace", func() {
					Expect(err).ToNot(HaveOccurred())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc1Name)).To(BeFalse())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace1, sc2Name)).To(BeFalse())
					Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace2, sc2Name)).To(BeTrue())
				})
			})
			When("there is not a match", func() {
				It("should no-op and not return an error", func() {
					Expect(err).ToNot(HaveOccurred())
				})
			})
		})
	})
})

var _ = Describe("DoesChanExistForStorageClass", func() {

	var (
		ctx       context.Context
		namespace string
		name      string
	)

	BeforeEach(func() {
		ctx = spqutil.NewContext()
		namespace = "default"
		name = "my-storage-class"
	})

	When("ctx is nil", func() {
		It("should panic", func() {
			fn := func() {
				spqutil.DoesChanExistForStorageClass(
					nil,
					namespace,
					name)
			}
			Expect(fn).To(PanicWith("ctx is nil"))
		})
	})

	When("namespace is empty", func() {
		It("should panic", func() {
			fn := func() {
				spqutil.DoesChanExistForStorageClass(
					ctx,
					"",
					name)
			}
			Expect(fn).To(PanicWith("namespace is empty"))
		})
	})

	When("name is empty", func() {
		It("should panic", func() {
			fn := func() {
				spqutil.DoesChanExistForStorageClass(
					ctx,
					namespace,
					"")
			}
			Expect(fn).To(PanicWith("name is empty"))
		})
	})

	When("channelMap is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				spqutil.DoesChanExistForStorageClass(
					context.Background(),
					namespace,
					name)
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})

	When("channel exists", func() {
		It("should return true", func() {
			_ = spqutil.ExecuteForStorageClass(
				ctx,
				namespace,
				name,
				noopExecuteFn)
			Expect(spqutil.DoesChanExistForStorageClass(ctx, namespace, name)).To(BeTrue())
		})
	})
})

var _ = Describe("ExecuteForStorageClass", func() {

	var (
		ctx       context.Context
		namespace string
		name      string
	)

	BeforeEach(func() {
		ctx = spqutil.NewContext()
		namespace = "default"
		name = "my-storage-class"
	})

	When("ctx is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.ExecuteForStorageClass(
					nil,
					namespace,
					name,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("ctx is nil"))
		})
	})

	When("namespace is empty", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.ExecuteForStorageClass(
					ctx,
					"",
					name,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("namespace is empty"))
		})
	})

	When("name is empty", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.ExecuteForStorageClass(
					ctx,
					namespace,
					"",
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("name is empty"))
		})
	})

	When("fn is nil", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.ExecuteForStorageClass(
					ctx,
					namespace,
					name,
					nil)
			}
			Expect(fn).To(PanicWith("fn is nil"))
		})
	})

	When("channelMap is missing from context", func() {
		It("should panic", func() {
			fn := func() {
				_ = spqutil.ExecuteForStorageClass(
					context.Background(),
					namespace,
					name,
					noopExecuteFn)
			}
			Expect(fn).To(PanicWith("map is missing from context"))
		})
	})

	When("the context is timed out or cancelled", func() {
		It("should not execute the function", func() {
			var i int64

			fn := func() {
				atomic.AddInt64(&i, 1)
			}

			// Create a cancelled context.
			ctx, cancel := context.WithCancel(ctx)
			cancel()

			// Attempt to increment i, n times.
			for i := 0; i < 5; i++ {
				_ = spqutil.ExecuteForStorageClass(
					ctx,
					namespace,
					name,
					fn,
				)
			}

			Consistently(i, time.Second*3).Should(Equal(int64(0)))
		})
	})

	When("there are multiple concurrent calls for the same namespace/storage class", func() {
		It("should execute the function exactly once", func() {
			var (
				i    int64
				incd = make(chan struct{})
				done = make(chan struct{})
				wait = make(chan struct{})
			)

			fn := func() {
				atomic.AddInt64(&i, 1)

				// Indicate the value has been added.
				close(incd)

				// Wait for the checks to complete before continuing.
				<-wait

				// Indicate that the test may complete.
				close(done)
			}

			// Attempt to increment i multiple times.
			numScheduled := 0
			for j := 0; j < 10; j++ {
				if spqutil.ExecuteForStorageClass(
					ctx,
					namespace,
					name,
					fn) {

					numScheduled++
				}
			}

			Expect(numScheduled).To(Equal(1))

			// Wait for the value to be incremented.
			<-incd

			// Assert that over a period of time, the value is not updated by
			// more than the single goroutine.
			Consistently(i, time.Second*3).Should(Equal(int64(1)))

			// Indicate to the goroutine it can complete.
			close(wait)

			// Wait for the goroutine to finish.
			<-done
		})
	})
})
