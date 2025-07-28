// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/vcenter"
	"github.com/vmware/govmomi/vim25"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	imgregv1a1 "github.com/vmware-tanzu/image-registry-operator-api/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
)

const (
	sourceVirtualMachineType = "VirtualMachine"

	itemDescriptionFormat = "virtualmachinepublishrequest.vmoperator.vmware.com: %s\n"
)

func CreateOVF(
	vmCtx pkgctx.VirtualMachineContext,
	client *rest.Client,
	vmPubReq *vmopv1.VirtualMachinePublishRequest,
	cl *imgregv1a1.ContentLibrary,
	actID string) (string, error) {

	// Use VM Operator specific description so that we can link published items
	// to the vmPub if anything unexpected happened.
	descriptionPrefix := fmt.Sprintf(itemDescriptionFormat, string(vmPubReq.UID))
	createSpec := vcenter.CreateSpec{
		Name:        vmPubReq.Status.TargetRef.Item.Name,
		Description: descriptionPrefix + vmPubReq.Status.TargetRef.Item.Description,
	}

	source := vcenter.ResourceID{
		Type:  sourceVirtualMachineType,
		Value: vmCtx.VM.Status.UniqueID,
	}

	target := vcenter.LibraryTarget{
		LibraryID: string(cl.Spec.UUID),
	}

	ovf := vcenter.OVF{
		Spec:   createSpec,
		Source: source,
		Target: target,
	}

	vmCtx.Logger.Info("Creating OVF from VM", "spec", ovf, "actId", actID)

	// Use vmpublish uid as the act id passed down to the content library service, so that we can track
	// the task status by the act id.
	return vcenter.NewManager(client).CreateOVF(
		pkgutil.WithVAPIActivationID(vmCtx, client, actID),
		ovf)
}

func CloneVM(
	vmCtx pkgctx.VirtualMachineContext,
	client *vim25.Client,
	vmPubReq *vmopv1.VirtualMachinePublishRequest,
	cl *imgregv1a1.ContentLibrary,
	actID string) (string, error) {

	cloneName := vmPubReq.Status.TargetRef.Item.Name

	folderRef := vimtypes.ManagedObjectReference{
		Type:  string(vimtypes.ManagedObjectTypesVirtualMachine),
		Value: string(cl.Spec.UUID),
	}

	// TODO(akutz) Figure out how encrypted VMs are to be handled.

	// TODO(akutz) Handle the async quota request.

	cloneSpec := vimtypes.VirtualMachineCloneSpec{
		Location: vimtypes.VirtualMachineRelocateSpec{
			Folder: &folderRef,

			// This causes a linked clone to be unlinked and consolidated.
			DiskMoveType: string(vimtypes.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndDisallowSharing),

			// TODO(akutz) Should be taken from the Image Registry v1alpha2
			//             ContentLibrary object.
			Profile: nil,
		},
		Template: true,

		// TODO(akutz) Are we going to support publishing VMs with their memory
		//             state intact?
		Snapshot: nil,
		Memory:   nil,

		// TODO(akutz) Use to disconnect NICs, clean ExtraConfig, etc. prior to
		//             cloning to a template.
		Config: nil,

		// TODO(akutz) Do we support publishing encrypted VMs at all?
		TpmProvisionPolicy: string(vimtypes.VirtualMachineCloneSpecTpmProvisionPolicyCopy),
	}

	vmCtx.Logger.Info("Publishing VM as template",
		"actId", actID,
		"cloneName", cloneName,
		"cloneSpec", cloneSpec,
		"cloneSrc", vmCtx.VM.Status.UniqueID,
		"cloneTgt", cl.Spec.UUID)

	vm := object.NewVirtualMachine(
		client,
		vimtypes.ManagedObjectReference{
			Type:  string(vimtypes.ManagedObjectTypesVirtualMachine),
			Value: vmCtx.VM.Status.UniqueID,
		})

	cloneTask, err := vm.Clone(
		vmCtx,
		object.NewFolder(client, folderRef),
		cloneName,
		cloneSpec)

	if err != nil {
		return "", fmt.Errorf("failed to call clone api: %w", err)
	}

	cloneTaskInfo, err := cloneTask.WaitForResult(vmCtx)
	if err != nil {
		return "", fmt.Errorf("failed to clone VM to template: %w", err)
	}

	vmCtx.Logger.V(4).Info("cloned vm", "taskInfo", cloneTaskInfo)

	cloneMoRef := cloneTaskInfo.Result.(vimtypes.ManagedObjectReference)
	return cloneMoRef.Value, nil
}
