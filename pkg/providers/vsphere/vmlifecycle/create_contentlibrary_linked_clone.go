// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package vmlifecycle

import (
	"errors"
	"fmt"
	"path"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vim25"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	clsutil "github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/contentlibrary"
)

func linkedCloneOVF(
	vmCtx pkgctx.VirtualMachineContext,
	k8sClient ctrlclient.Client,
	vimClient *vim25.Client,
	datacenter *object.Datacenter,
	item *library.Item,
	createArgs *CreateArgs) (*vimtypes.ManagedObjectReference, error) {

	logger := vmCtx.Logger.WithName("linkedCloneOVF")

	if len(createArgs.Datastores) == 0 {
		return nil, errors.New("no compatible datastores")
	}

	imgInfo, err := vmopv1util.GetImageLinkedCloneInfo(
		vmCtx,
		k8sClient,
		*vmCtx.VM.Spec.Image,
		vmCtx.VM.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get image linked clone info: %w", err)
	}

	dstDir := clsutil.GetCachedDirForLibraryItem(
		createArgs.Datastores[0].Name,
		imgInfo.ItemID,
		imgInfo.ItemContentVersion)
	logger.Info("Got cached dir", "dstDir", dstDir)

	dstURIs, err := clsutil.CacheStorageURIs(
		vmCtx,
		vimClient,
		datacenter.Reference(),
		datacenter.Reference(),
		createArgs.Datastores[0].MoRef,
		createArgs.Datastores[0].Name,
		dstDir,
		imgInfo.DiskURIs...)
	if err != nil {
		return nil, fmt.Errorf("failed to cache library item disks: %w", err)
	}
	logger.Info("Got parent disks", "dstURIs", dstURIs)

	vmDir := path.Dir(createArgs.ConfigSpec.Files.VmPathName)
	logger.Info("Got vm dir", "vmDir", vmDir)

	// Update the ConfigSpec with the disk chains.
	var disks []*vimtypes.VirtualDisk
	for i := range createArgs.ConfigSpec.DeviceChange {
		dc := createArgs.ConfigSpec.DeviceChange[i].GetVirtualDeviceConfigSpec()
		if d, ok := dc.Device.(*vimtypes.VirtualDisk); ok {
			disks = append(disks, d)

			// The profile is no longer needed since we have placement.
			dc.Profile = nil
		}
	}
	logger.Info("Got disks", "disks", disks)

	if a, b := len(dstURIs), len(disks); a != b {
		return nil, fmt.Errorf(
			"invalid disk count: len(uris)=%d, len(disks)=%d", a, b)
	}

	for i := range disks {
		d := disks[i]
		if bfb, ok := d.Backing.(vimtypes.BaseVirtualDeviceFileBackingInfo); ok {
			fb := bfb.GetVirtualDeviceFileBackingInfo()
			fb.Datastore = &createArgs.Datastores[0].MoRef
			fb.FileName = fmt.Sprintf("%s/%s-%d.vmdk", vmDir, vmCtx.VM.Name, i)
		}
		if fb, ok := d.Backing.(*vimtypes.VirtualDiskFlatVer2BackingInfo); ok {
			fb.Parent = &vimtypes.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: vimtypes.VirtualDeviceFileBackingInfo{
					Datastore: &createArgs.Datastores[0].MoRef,
					FileName:  dstURIs[i],
				},
				DiskMode:        string(vimtypes.VirtualDiskModePersistent),
				ThinProvisioned: ptr.To(true),
			}
		}
	}

	// The profile is no longer needed since we have placement.
	createArgs.ConfigSpec.VmProfile = nil

	vmCtx.Logger.Info(
		"Deploying OVF Library Item as linked clone",
		"itemID", item.ID,
		"itemName", item.Name,
		"configSpec", createArgs.ConfigSpec)

	folder := object.NewFolder(
		vimClient,
		vimtypes.ManagedObjectReference{
			Type:  "Folder",
			Value: createArgs.FolderMoID,
		})
	pool := object.NewResourcePool(
		vimClient,
		vimtypes.ManagedObjectReference{
			Type:  "ResourcePool",
			Value: createArgs.ResourcePoolMoID,
		})

	createTask, err := folder.CreateVM(
		vmCtx,
		createArgs.ConfigSpec,
		pool,
		nil)
	if err != nil {
		return nil, fmt.Errorf("failed to call create task: %w", err)
	}

	createTaskInfo, err := createTask.WaitForResult(vmCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for create task: %w", err)
	}

	vmRef, ok := createTaskInfo.Result.(vimtypes.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf(
			"failed to assert create task result is ref: %[1]T %+[1]v",
			createTaskInfo.Result)
	}

	return &vmRef, nil
}
