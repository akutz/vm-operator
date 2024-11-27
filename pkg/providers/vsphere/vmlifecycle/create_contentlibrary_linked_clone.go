// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package vmlifecycle

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	imgregv1a1 "github.com/vmware-tanzu/image-registry-operator-api/api/v1alpha1"
	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha3"
	"github.com/vmware-tanzu/vm-operator/pkg/conditions"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
)

func linkedCloneOVF(
	vmCtx pkgctx.VirtualMachineContext,
	k8sClient ctrlclient.Client,
	vimClient *vim25.Client,
	restClient *rest.Client,
	finder *find.Finder,
	datacenter *object.Datacenter,
	item *library.Item,
	createArgs *CreateArgs) (*vimtypes.ManagedObjectReference, error) {

	imageRef := vmCtx.VM.Spec.Image
	if imageRef == nil {
		panic("nil image ref")
	}

	storageURI, guestID, err := getBackingFileNameByImageRef(
		vmCtx,
		library.NewManager(restClient),
		k8sClient,
		true,
		*imageRef)
	if err != nil {
		return nil, err
	}

	datastoreName, err := getDatastoreNameFromStorageURI(storageURI)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get datastore name from storage uri %q: %w",
			storageURI, err)
	}

	datastore, err := finder.Datastore(vmCtx, datastoreName)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get datastore from name %q: %w", datastoreName, err)
	}

	vmPath := fmt.Sprintf("[%s] %s", datastore.Name(), vmCtx.VM.Name)
	vmDiskPath := fmt.Sprintf("%s/%s.vmdk", vmPath, vmCtx.VM.Name)

	srcDiskName := path.Base(storageURI)
	tgtDiskDir := fmt.Sprintf("[%s]/.vmservice", datastoreName)
	tgtDiskPath := path.Join(tgtDiskDir, srcDiskName)

	// Create the base disk if it does not exist.
	vdiskManager := newVirtualDiskManager(vimClient)
	if _, err := vdiskManager.QueryVirtualDiskUuid(
		vmCtx,
		tgtDiskPath,
		datacenter); err != nil {

		if !fault.Is(err, &vimtypes.FileNotFound{}) {
			return nil, fmt.Errorf("failed to query disk uuid: %w", err)
		}

		// Create the VM folder.
		fileManager := object.NewFileManager(vimClient)
		if err := fileManager.MakeDirectory(
			vmCtx,
			tgtDiskDir,
			datacenter,
			true,
		); err != nil {
			return nil, fmt.Errorf(
				"failed to create vmservice folder %q: %w", tgtDiskDir, err)
		}

		// The base disk does not exist, create it.
		copyDiskTask, err := vdiskManager.CopyVirtualDisk(
			vmCtx,
			storageURI,
			datacenter,
			tgtDiskPath,
			datacenter,
			&vimtypes.FileBackedVirtualDiskSpec{
				VirtualDiskSpec: vimtypes.VirtualDiskSpec{
					AdapterType: string(vimtypes.VirtualDiskAdapterTypeLsiLogic),
					DiskType:    string(vimtypes.VirtualDiskTypeThin),
				},
			},
			false)
		if err != nil {
			return nil, fmt.Errorf("failed to call copy disk: %w", err)
		}
		if err := copyDiskTask.Wait(vmCtx); err != nil {
			return nil, fmt.Errorf("failed to wait for copy disk: %w", err)
		}
	}

	configSpec := createArgs.ConfigSpec

	configSpec.Files = &types.VirtualMachineFileInfo{
		VmPathName: vmPath,
	}

	if configSpec.GuestId == "" {
		configSpec.GuestId = guestID
	}

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

	var (
		scsiControllerKey int32
		maxKey            int32
	)
	for i := range configSpec.DeviceChange {
		dcs := configSpec.DeviceChange[i].GetVirtualDeviceConfigSpec()
		if scsiControllerKey == 0 {
			if d, ok := dcs.Device.(*vimtypes.ParaVirtualSCSIController); ok {
				scsiControllerKey = d.Key
			}
		}
		if key := dcs.Device.GetVirtualDevice().Key; key < maxKey {
			maxKey = key
		}
	}

	if scsiControllerKey == 0 {
		scsiControllerKey = maxKey - 1
		configSpec.DeviceChange = append(
			configSpec.DeviceChange,
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device: &types.ParaVirtualSCSIController{
					VirtualSCSIController: types.VirtualSCSIController{
						SharedBus:          types.VirtualSCSISharingNoSharing,
						ScsiCtlrUnitNumber: 7,
						VirtualController: types.VirtualController{
							BusNumber: 0,
							VirtualDevice: types.VirtualDevice{
								Key:           scsiControllerKey,
								ControllerKey: 100,
								UnitNumber:    ptr.To(int32(3)),
							},
						},
					},
				},
			},
		)
	}

	configSpec.DeviceChange = append(
		configSpec.DeviceChange,
		&vimtypes.VirtualDeviceConfigSpec{
			Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
			FileOperation: vimtypes.VirtualDeviceConfigSpecFileOperationCreate,
			Device: &vimtypes.VirtualDisk{
				VirtualDevice: vimtypes.VirtualDevice{
					ControllerKey: scsiControllerKey,
					Key:           maxKey - 2,
					UnitNumber:    ptr.To(int32(0)),
					Backing: &vimtypes.VirtualDiskFlatVer2BackingInfo{
						Parent: &vimtypes.VirtualDiskFlatVer2BackingInfo{
							VirtualDeviceFileBackingInfo: vimtypes.VirtualDeviceFileBackingInfo{
								Datastore: ptr.To(datastore.Reference()),
								FileName:  tgtDiskPath,
							},
							ThinProvisioned: ptr.To(false),
							WriteThrough:    ptr.To(false),
							DiskMode:        string(types.VirtualDiskModePersistent),
						},
						VirtualDeviceFileBackingInfo: vimtypes.VirtualDeviceFileBackingInfo{
							Datastore: ptr.To(datastore.Reference()),
							FileName:  vmDiskPath,
						},
						DiskMode:        string(vimtypes.VirtualDiskModePersistent),
						ThinProvisioned: ptr.To(true),
					},
				},
			},
		})

	vmCtx.Logger.Info(
		"Deploying OVF Library Item as linked clone",
		"itemID", item.ID,
		"itemName", item.Name,
		"configSpec", configSpec)

	createTask, err := folder.CreateVM(
		vmCtx,
		configSpec,
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

var dsNameRX = regexp.MustCompile(`^\[([^\]].+)\].*$`)

func getDatastoreNameFromStorageURI(s string) (string, error) {
	m := dsNameRX.FindStringSubmatch(s)
	if len(m) == 0 {
		return "", fmt.Errorf("no matches found for %q", s)
	}
	return m[1], nil
}

const (
	vmiKind  = "VirtualMachineImage"
	cvmiKind = "Cluster" + vmiKind
)

// getBackingFileNameByImageRef returns the VMDK type content library file name
// based on the given VirtualMachineImageRef. It also syncs the content library
// if needed to ensure the file is available.
func getBackingFileNameByImageRef(
	vmCtx pkgctx.VirtualMachineContext,
	libManager *library.Manager,
	client ctrlclient.Client,
	syncFile bool,
	imageRef vmopv1.VirtualMachineImageRef) (string, string, error) {

	var (
		libItemUUID string
		itemStatus  imgregv1a1.ContentLibraryItemStatus
		imageStatus vmopv1.VirtualMachineImageStatus
		err         error
	)

	switch imageRef.Kind {
	case vmiKind:
		libItemUUID, imageStatus, itemStatus, err = processImage(vmCtx, client, imageRef.Name, vmCtx.VM.Namespace)
		if err != nil {
			return "", "", err
		}
	case cvmiKind:
		libItemUUID, imageStatus, itemStatus, err = processImage(vmCtx, client, imageRef.Name, "")
		if err != nil {
			return "", "", err
		}
	default:
		return "", "", fmt.Errorf("unsupported image kind: %q", imageRef.Kind)
	}

	var storageURI string
	for i := range itemStatus.FileInfo {
		fi := itemStatus.FileInfo[i]
		if fi.StorageURI != "" {
			if path.Ext(fi.StorageURI) == ".vmdk" {
				storageURI = fi.StorageURI
				break
			}
		}
	}
	if storageURI == "" {
		return "", "", fmt.Errorf(
			"no storage URI found in the content library item status: %v",
			itemStatus)
	}

	// A file from a library item that belongs to a subscribed library may not
	// be fully available. Sync the file to ensure it is present.
	if syncFile && (!itemStatus.Cached || itemStatus.SizeInBytes.Size() == 0) {
		vmCtx.Logger.Info("Syncing content library item", "libItemUUID", libItemUUID)
		libItem, err := libManager.GetLibraryItem(vmCtx, libItemUUID)
		if err != nil {
			return "", "", fmt.Errorf("error getting library item %s to sync: %w", libItemUUID, err)
		}
		if err := libManager.SyncLibraryItem(vmCtx, libItem, true); err != nil {
			return "", "", fmt.Errorf("error syncing library item %s: %w", libItemUUID, err)
		}
	}

	return storageURI, imageStatus.OSInfo.Type, nil
}

// processImage validates if the image is ready and returns the content library
// item UUID and status from its provider reference.
func processImage(
	ctx context.Context,
	client ctrlclient.Client,
	name, namespace string) (string, vmopv1.VirtualMachineImageStatus, imgregv1a1.ContentLibraryItemStatus, error) {

	var vmi vmopv1.VirtualMachineImage

	if namespace != "" {
		// Namespace scope image.
		if err := client.Get(
			ctx,
			ctrlclient.ObjectKey{
				Name:      name,
				Namespace: namespace,
			},
			&vmi); err != nil {

			return "",
				vmopv1.VirtualMachineImageStatus{},
				imgregv1a1.ContentLibraryItemStatus{},
				err
		}
	} else {
		// Cluster scope image.
		var cvmi vmopv1.ClusterVirtualMachineImage
		if err := client.Get(
			ctx,
			ctrlclient.ObjectKey{
				Name: name,
			}, &cvmi); err != nil {

			return "",
				vmopv1.VirtualMachineImageStatus{},
				imgregv1a1.ContentLibraryItemStatus{},
				err
		}

		vmi = vmopv1.VirtualMachineImage(cvmi)
	}

	// Verify image before retrieving content library item.
	if !conditions.IsTrue(&vmi, vmopv1.ReadyConditionType) {
		return "", vmopv1.VirtualMachineImageStatus{},
			imgregv1a1.ContentLibraryItemStatus{}, fmt.Errorf(
				"image condition is not ready: %v",
				conditions.Get(&vmi, vmopv1.ReadyConditionType))
	}
	if vmi.Status.Type != string(imgregv1a1.ContentLibraryItemTypeOvf) {
		return "",
			vmopv1.VirtualMachineImageStatus{},
			imgregv1a1.ContentLibraryItemStatus{},
			fmt.Errorf(
				"image type %q is not OVF", vmi.Status.Type)
	}
	if vmi.Spec.ProviderRef == nil || vmi.Spec.ProviderRef.Name == "" {
		return "",
			vmopv1.VirtualMachineImageStatus{},
			imgregv1a1.ContentLibraryItemStatus{},
			errors.New(
				"image provider ref is empty")
	}

	var (
		itemName = vmi.Spec.ProviderRef.Name
		clitem   imgregv1a1.ContentLibraryItem
	)

	if namespace != "" {
		// Namespace scope ContentLibraryItem.
		if err := client.Get(
			ctx,
			ctrlclient.ObjectKey{
				Name:      itemName,
				Namespace: namespace,
			},
			&clitem); err != nil {

			return "",
				vmopv1.VirtualMachineImageStatus{},
				imgregv1a1.ContentLibraryItemStatus{},
				err
		}
	} else {
		// Cluster scope ClusterContentLibraryItem.
		var cclitem imgregv1a1.ClusterContentLibraryItem
		if err := client.Get(
			ctx,
			ctrlclient.ObjectKey{Name: itemName},
			&cclitem); err != nil {

			return "",
				vmopv1.VirtualMachineImageStatus{},
				imgregv1a1.ContentLibraryItemStatus{},
				err
		}
		clitem = imgregv1a1.ContentLibraryItem(cclitem)
	}

	return string(clitem.Spec.UUID), vmi.Status, clitem.Status, nil
}

type virtualDiskManager struct {
	*object.VirtualDiskManager
}

func newVirtualDiskManager(c *vim25.Client) *virtualDiskManager {
	m := virtualDiskManager{
		VirtualDiskManager: object.NewVirtualDiskManager(c),
	}
	return &m
}

func (m virtualDiskManager) CopyVirtualDisk(
	ctx context.Context,
	sourceName string, sourceDatacenter *object.Datacenter,
	destName string, destDatacenter *object.Datacenter,
	destSpec vimtypes.BaseVirtualDiskSpec, force bool) (*object.Task, error) {

	req := vimtypes.CopyVirtualDisk_Task{
		This:       m.Reference(),
		SourceName: sourceName,
		DestName:   destName,
		DestSpec:   destSpec,
		Force:      vimtypes.NewBool(force),
	}

	if sourceDatacenter != nil {
		ref := sourceDatacenter.Reference()
		req.SourceDatacenter = &ref
	}

	if destDatacenter != nil {
		ref := destDatacenter.Reference()
		req.DestDatacenter = &ref
	}

	res, err := methods.CopyVirtualDisk_Task(ctx, m.Client(), &req)
	if err != nil {
		return nil, err
	}

	return object.NewTask(m.Client(), res.Returnval), nil
}
