// +build integration

/* **********************************************************
 * Copyright 2019 VMware, Inc.  All rights reserved. -- VMware Confidential
 * **********************************************************/

package virtualmachinesetresourcepolicy

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/net/context"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	vmoperatorv1alpha1 "github.com/vmware-tanzu/vm-operator/pkg/apis/vmoperator/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/test/integration"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var c client.Client

const timeout = time.Second * 5

var _ = Describe("VirtualMachineSetResourcePolicy controller", func() {
	name := "fooVm"
	ns := integration.DefaultNamespace

	var (
		instance                vmoperatorv1alpha1.VirtualMachineSetResourcePolicy
		invalid                 vmoperatorv1alpha1.VirtualMachineSetResourcePolicy
		stopMgr                 chan struct{}
		mgrStopped              *sync.WaitGroup
		mgr                     manager.Manager
		err                     error
		leaderElectionConfigMap string
	)

	BeforeEach(func() {
		// Setup the Manager and Controller.  Wrap the Controller Reconcile function so it writes each request to a
		// channel when it is finished.
		syncPeriod := 10 * time.Second
		leaderElectionConfigMap = fmt.Sprintf("vmoperator-controller-manager-runtime-%s", uuid.New())
		mgr, err = manager.New(cfg, manager.Options{SyncPeriod: &syncPeriod,
			LeaderElection:          true,
			LeaderElectionID:        leaderElectionConfigMap,
			LeaderElectionNamespace: ns})
		Expect(err).NotTo(HaveOccurred())
		c = mgr.GetClient()

		stopMgr, mgrStopped = StartTestManager(mgr)
	})

	AfterEach(func() {
		close(stopMgr)
		mgrStopped.Wait()
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      leaderElectionConfigMap,
			},
		}
		err := c.Delete(context.Background(), configMap)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("when creating/deleting a VirtualMachineSetResourcePolicy", func() {
		It("invoke the validate method", func() {
			// Create the VirtualMachineSetResourcePolicy object and expect this to fail
			invalid = vmoperatorv1alpha1.VirtualMachineSetResourcePolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns,
				},
				Spec: vmoperatorv1alpha1.VirtualMachineSetResourcePolicySpec{
					ResourcePool: vmoperatorv1alpha1.ResourcePoolSpec{
						Name: "name-resourcepool",
						Reservations: vmoperatorv1alpha1.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("1000Mi"),
							Memory: resource.MustParse("200Mi"),
						},
						Limits: vmoperatorv1alpha1.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("2000Mi"),
							Memory: resource.MustParse("100Mi"),
						},
					},
					Folder: vmoperatorv1alpha1.FolderSpec{
						Name: "name-folder",
					},
				},
			}

			err = c.Create(context.TODO(), &invalid)
			Expect(err).To(HaveOccurred())

			err = c.Delete(context.TODO(), &invalid)
			Expect(err).To(HaveOccurred())
		})

		It("invoke the reconcile method", func() {
			instance = vmoperatorv1alpha1.VirtualMachineSetResourcePolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns,
				},
				Spec: vmoperatorv1alpha1.VirtualMachineSetResourcePolicySpec{
					ResourcePool: vmoperatorv1alpha1.ResourcePoolSpec{
						Name: "name-resourcepool",
						Reservations: vmoperatorv1alpha1.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("1000Mi"),
							Memory: resource.MustParse("100Mi"),
						},
						Limits: vmoperatorv1alpha1.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("2000Mi"),
							Memory: resource.MustParse("200Mi"),
						},
					},
					Folder: vmoperatorv1alpha1.FolderSpec{
						Name: "name-folder",
					},
				},
			}

			expectedRequest := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
			recFn, requests := SetupTestReconcile(newReconciler(mgr))
			Expect(add(mgr, recFn)).To(Succeed())
			// Create the VirtualMachineSetResourcePolicy object and expect the Reconcile
			err = c.Create(context.TODO(), &instance)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
			// Delete the VirtualMachineSetResourcePolicy object and expect the Reconcile
			err = c.Delete(context.TODO(), &instance)
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
		})
	})
})