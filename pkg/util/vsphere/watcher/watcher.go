// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

type Result struct {
	Namespace    string
	NamespaceRef types.ManagedObjectReference
	VmName       string
	VmRef        types.ManagedObjectReference
}

// Start begins watching a vSphere server for updates to VMs.
func Start(
	ctx context.Context,
	client *vim25.Client,
	datacenterRef,
	namespacesFolderRef types.ManagedObjectReference) (<-chan Result, <-chan error) {

	var (
		chanErr = make(chan error)
		chanRes = make(chan Result)
	)

	go func() {
		defer func() {
			close(chanRes)
			close(chanErr)
		}()

		// Get the default property collector.
		dpc := property.DefaultCollector(client)

		// Create a new property collector to watch for changes.
		pc, err := dpc.Create(ctx)
		if err != nil {
			chanErr <- err
			return
		}
		defer pc.Destroy(context.TODO())

		// Create a property filter that watches all VMs recursively under the
		// specified folder and receives updates for VMs that match the filter.
		pf, err := pc.CreateFilter(ctx, createFilter(namespacesFolderRef))
		if err != nil {
			chanErr <- err
			return
		}
		defer pf.Destroy(context.TODO())

		if err := pc.WaitForUpdatesEx(
			ctx,
			property.WaitOptions{},
			func(ou []types.ObjectUpdate) bool {
				for i := range ou {
					if err := onUpdate(
						ctx,
						client,
						ou[i],
						chanRes); err != nil {

						chanErr <- err
						return true
					}
				}
				return false
			}); err != nil {

			chanErr <- err
			return
		}
	}()

	return chanRes, chanErr
}

func onUpdate(
	ctx context.Context,
	vimClient *vim25.Client,
	ou types.ObjectUpdate,
	chanResult chan Result) error {

	// Only pay attention to enter and modify events.
	if ou.Kind != types.ObjectUpdateKindEnter &&
		ou.Kind != types.ObjectUpdateKindModify {

		return nil
	}

	obj := object.NewVirtualMachine(vimClient, ou.Obj)
	var objMo mo.VirtualMachine
	if err := obj.Properties(
		ctx,
		ou.Obj,
		[]string{"name", "parent"},
		&objMo); err != nil {

		return err
	}

	if objMo.Parent == nil {
		return fmt.Errorf("parent is nil")
	}

	objParent := object.NewFolder(vimClient, *objMo.Parent)
	var objParentMo mo.Folder
	if err := objParent.Properties(
		ctx,
		*objMo.Parent,
		[]string{"namespace"},
		&objParentMo); err != nil {

		return err
	}

	if objParentMo.Namespace == nil {
		// Does not belong to a namespace.
		return nil
	}

	// Notify the channel about the result.
	chanResult <- Result{
		Namespace:    *objParentMo.Namespace,
		NamespaceRef: *objMo.Parent,
		VmName:       objMo.Name,
		VmRef:        ou.Obj,
	}

	return nil
}

func createFilter(
	folderRef types.ManagedObjectReference) types.CreateFilter {

	return types.CreateFilter{
		Spec: types.PropertyFilterSpec{
			ObjectSet: []types.ObjectSpec{
				{
					Obj:  folderRef,
					Skip: &[]bool{true}[0],
					SelectSet: []types.BaseSelectionSpec{
						// Folder --> children (folder / VM)
						&types.TraversalSpec{
							SelectionSpec: types.SelectionSpec{
								Name: "visitFolders",
							},
							Type: "Folder",
							// Folder --> children (folder / VM)
							Path: "childEntity",
							SelectSet: []types.BaseSelectionSpec{
								// Folder --> child folder
								&types.SelectionSpec{
									Name: "visitFolders",
								},
							},
						},
					},
				},
			},
			PropSet: []types.PropertySpec{
				{
					Type: "VirtualMachine",
					PathSet: []string{
						"config.changeVersion",
						"guest.ipStack",
						"guest.net",
						"summary.guest",
						"summary.overallStatus",
						"summary.runtime.host",
						"summary.runtime.powerState",
					},
				},
			},
		},
	}
}
