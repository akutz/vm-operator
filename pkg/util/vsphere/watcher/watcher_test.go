// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package watcher_test

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/watcher"
)

var _ = Describe("Start", func() {

	var (
		ctx       context.Context
		cancelCtx context.Context
		cancel    context.CancelFunc
		model     *simulator.Model
		server    *simulator.Server
		client    *vim25.Client

		datacenterRef types.ManagedObjectReference
		namespacesRef types.ManagedObjectReference

		datastore        *object.Datastore
		pool             *object.ResourcePool
		vmFolder         *object.Folder
		namespacesFolder *object.Folder
		ns1Folder        *object.Folder

		chanErr    <-chan error
		chanResult <-chan watcher.Result

		createVmBeforeStartingWatcher bool
	)

	assertTask := func(t *object.Task, err error) *types.TaskInfo {
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ti, err := t.WaitForResultEx(ctx)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		ExpectWithOffset(1, ti.Error).To(BeNil())
		return ti
	}

	BeforeEach(func() {
		ctx = logr.NewContext(context.TODO(), GinkgoLogr)
		cancelCtx, cancel = context.WithCancel(ctx)

		// Start the simulator.
		model = simulator.VPX()
		model.Autostart = false
		model.Host = 0
		model.Cluster = 1
		Expect(model.Create()).To(Succeed())
		model.Service.RegisterEndpoints = true
		model.Service.TLS = &tls.Config{}
		server = model.Service.NewServer()

		// Get a client for the simulator.
		c, err := govmomi.NewClient(ctx, server.URL, true)
		Expect(err).ToNot(HaveOccurred())
		Expect(c).ToNot(BeNil())
		client = c.Client

		finder := find.NewFinder(client)
		dc, err := finder.DefaultDatacenter(ctx)
		Expect(err).ToNot(HaveOccurred())
		finder.SetDatacenter(dc)
		datacenterRef = dc.Reference()

		// Get the VM folder.
		vmf, err := finder.Folder(ctx, fmt.Sprintf("/%s/vm", dc.Name()))
		Expect(err).ToNot(HaveOccurred())
		vmFolder = vmf

		// Get the datastore.
		ds, err := finder.DefaultDatastore(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(ds).ToNot(BeNil())
		datastore = ds

		// Get the resource pool.
		ccr, err := finder.DefaultClusterComputeResource(ctx)
		Expect(err).ToNot(HaveOccurred())
		rp, err := ccr.ResourcePool(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(rp).ToNot(BeNil())
		pool = rp

		// Create the namespaces folder.
		nsf, err := vmf.CreateFolder(ctx, "Namespaces")
		Expect(err).ToNot(HaveOccurred())
		Expect(nsf).ToNot(BeNil())
		namespacesRef = nsf.Reference()
		namespacesFolder = nsf

		// Create the namespace ns1.
		ns1f, err := namespacesFolder.CreateFolder(ctx, "ns1")
		Expect(err).ToNot(HaveOccurred())
		Expect(ns1f).ToNot(BeNil())
		simulator.Map.Get(ns1f.Reference()).(*simulator.Folder).Namespace = &[]string{"ns1"}[0]
		ns1Folder = ns1f
		ns1Folder.SetInventoryPath(fmt.Sprintf("/%s/vm/Namespaces/ns1", dc.Name()))
	})

	JustBeforeEach(func() {
		if !createVmBeforeStartingWatcher {
			// Start the watcher.
			chanResult, chanErr = watcher.Start(
				cancelCtx,
				client,
				datacenterRef,
				namespacesRef)
		}
	})

	AfterEach(func() {
		Consistently(chanErr).ShouldNot(Receive())
		Consistently(chanResult).ShouldNot(Receive())
		cancel()
		<-chanErr
		<-chanResult
		server.Close()
		model = nil
		server = nil
		client = nil
		chanErr = nil
		chanResult = nil
	})

	When("there are no changes, tasks, or events", func() {
		Specify("the watcher should be silent", func() {
			Consistently(chanResult).ShouldNot(Receive())
		})
	})

	When("a vm is created", func() {
		var (
			parent *object.Folder
			vm     *object.VirtualMachine
		)

		JustBeforeEach(func() {
			// Create the VM.
			ti := assertTask(parent.CreateVM(
				ctx,
				types.VirtualMachineConfigSpec{
					Name: "my-vm",
					Files: &types.VirtualMachineFileInfo{
						VmPathName: fmt.Sprintf("[%s] %s", datastore.Name(), "my-vm"),
					},
				}, pool, nil))
			vmRef := ti.Result.(types.ManagedObjectReference)
			vm = object.NewVirtualMachine(client, vmRef)

			if createVmBeforeStartingWatcher {
				// Start the watcher.
				chanResult, chanErr = watcher.Start(
					cancelCtx,
					client,
					datacenterRef,
					namespacesRef)
			}
		})

		Context("outside of the namespaces folder", func() {
			BeforeEach(func() {
				parent = vmFolder
			})
			Specify("the watcher should be silent", func() {
				Consistently(chanResult).ShouldNot(Receive())
			})
		})

		Context("in namespace ns1", func() {
			var (
				result watcher.Result
			)

			BeforeEach(func() {
				parent = ns1Folder
			})

			JustBeforeEach(func() {
				result = watcher.Result{
					Namespace:    parent.Name(),
					NamespaceRef: parent.Reference(),
					VmName:       "my-vm",
					VmRef:        vm.Reference(),
				}
			})

			Context("before the watcher is started", func() {
				BeforeEach(func() {
					createVmBeforeStartingWatcher = true
				})

				Specify("a notification should be received", func() {
					Eventually(chanResult).Should(Receive(Equal(result)))
				})
				When("the vm is modified", func() {
					Specify("one notification should be received", func() {
						assertTask(
							vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{
								ExtraConfig: []types.BaseOptionValue{
									&types.OptionValue{
										Key:   "hello",
										Value: "world",
									},
								},
							}),
						)
						Eventually(chanResult).Should(Receive(Equal(result)))
					})
				})
				When("the vm is powered on", func() {
					Specify("one notification should be received", func() {
						assertTask(vm.PowerOn(ctx))
						Eventually(chanResult).Should(Receive(Equal(result)))
					})
				})
			})

			Context("after the watcher is started", func() {
				Specify("a notification should be received", func() {
					Eventually(chanResult).Should(Receive(Equal(result)))
				})
				When("the vm is modified", func() {
					Specify("two notifications should be received", func() {
						Eventually(chanResult).Should(Receive(Equal(result)))
						assertTask(
							vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{
								ExtraConfig: []types.BaseOptionValue{
									&types.OptionValue{
										Key:   "hello",
										Value: "world",
									},
								},
							}),
						)
						Eventually(chanResult).Should(Receive(Equal(result)))
					})
				})
				When("the vm is powered on", func() {
					Specify("two notifications should be received", func() {
						Eventually(chanResult).Should(Receive(Equal(result)))
						assertTask(vm.PowerOn(ctx))
						Eventually(chanResult).Should(Receive(Equal(result)))
					})
				})
			})
		})
	})
})
