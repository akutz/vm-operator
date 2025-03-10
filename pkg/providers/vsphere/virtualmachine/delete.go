// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/vmware/govmomi/object"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/util/paused"
	vmutil "github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/vm"
)

// errorVMPausedByAdmin is an error thrown during VM deletion.
// Indicating because admin paused VM, deletion operation is paused.
var errorVMPausedByAdmin = errors.New(constants.VMPausedByAdminError)

func ErrorVMPausedByAdmin() error {
	return errorVMPausedByAdmin
}

func DeleteVirtualMachine(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) error {

	if err := vcVM.Properties(
		vmCtx,
		vcVM.Reference(),
		[]string{"config.extraConfig"}, &vmCtx.MoVM); err != nil {

		vmCtx.Logger.Error(err, "failed to fetch config.extraConfig properties of VM for DeleteVirtualMachine")
		return err
	}
	// Throw an error to distinguish from successful deletion.
	if paused := paused.ByAdmin(vmCtx.MoVM); paused {
		if vmCtx.VM.Labels == nil {
			vmCtx.VM.Labels = make(map[string]string)
		}
		vmCtx.VM.Labels[vmopv1.PausedVMLabelKey] = "admin"
		return ErrorVMPausedByAdmin()
	}
	if _, err := vmutil.SetAndWaitOnPowerState(
		logr.NewContext(vmCtx, vmCtx.Logger),
		vcVM.Client(),
		vmutil.ManagedObjectFromObject(vcVM),
		false,
		vimtypes.VirtualMachinePowerStatePoweredOff,
		vmutil.ParsePowerOpMode(string(vmCtx.VM.Spec.PowerOffMode))); err != nil {

		return err
	}

	t, err := vcVM.Destroy(vmCtx)
	if err != nil {
		return err
	}

	if taskInfo, err := t.WaitForResult(vmCtx); err != nil {
		if taskInfo != nil {
			vmCtx.Logger.V(5).Error(err, "destroy VM task failed", "taskInfo", taskInfo)
		}
		return fmt.Errorf("destroy VM task failed: %w", err)
	}

	return nil
}
