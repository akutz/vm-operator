// Copyright (c) 2022 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"k8s.io/utils/pointer"

	vimtypes "github.com/vmware/govmomi/vim25/types"

	"github.com/vmware-tanzu/vm-operator/api/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/lib"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/instancestorage"
)

func CreateConfigSpec(
	name string,
	vmClassSpec *v1alpha1.VirtualMachineClassSpec,
	minFreq uint64,
	firmware string, hardwareVersion int,
	baseConfigSpec *vimtypes.VirtualMachineConfigSpec,
) vimtypes.VirtualMachineConfigSpec {

	var configSpec vimtypes.VirtualMachineConfigSpec
	if baseConfigSpec != nil {
		configSpec = *baseConfigSpec
	}

	configSpec.Name = name

	if configSpec.Annotation == "" {
		configSpec.Annotation = constants.VCVMAnnotation
	}

	// CPU and Memory configurations specified in the VM Class spec.hardware
	// takes precedence over values in the config spec
	// TODO: might need revisiting to conform on a single way of consuming cpu and memory.
	//  we prefer the vm class CR to have consistent values to reduce confusion.
	if configSpec.NumCPUs == 0 {
		configSpec.NumCPUs = int32(vmClassSpec.Hardware.Cpus)
	}
	if configSpec.MemoryMB == 0 {
		configSpec.MemoryMB = MemoryQuantityToMb(vmClassSpec.Hardware.Memory)
	}

	// TODO: add constants for this key.
	configSpec.ManagedBy = &vimtypes.ManagedByInfo{
		ExtensionKey: "com.vmware.vcenter.wcp",
		Type:         "VirtualMachine",
	}

	if minFreq > 0 {
		cpuReq := vmClassSpec.Policies.Resources.Requests.Cpu
		cpuLim := vmClassSpec.Policies.Resources.Limits.Cpu
		if !cpuReq.IsZero() || !cpuLim.IsZero() {
			if configSpec.CpuAllocation == nil {
				configSpec.CpuAllocation = &vimtypes.ResourceAllocationInfo{}
			}
			if configSpec.CpuAllocation.Shares == nil {
				configSpec.CpuAllocation.Shares = &vimtypes.SharesInfo{
					Level: vimtypes.SharesLevelNormal,
				}
			}
			if v := cpuReq; !v.IsZero() {
				rsv := CPUQuantityToMhz(v, minFreq)
				configSpec.CpuAllocation.Reservation = &rsv
			}
			if v := cpuLim; !v.IsZero() {
				rsv := CPUQuantityToMhz(v, minFreq)
				configSpec.CpuAllocation.Limit = &rsv
			}
		}
	}

	memReq := vmClassSpec.Policies.Resources.Requests.Memory
	memLim := vmClassSpec.Policies.Resources.Limits.Memory
	if !memReq.IsZero() || !memLim.IsZero() {
		if configSpec.MemoryAllocation == nil {
			configSpec.MemoryAllocation = &vimtypes.ResourceAllocationInfo{}
		}
		if configSpec.MemoryAllocation.Shares == nil {
			configSpec.MemoryAllocation.Shares = &vimtypes.SharesInfo{
				Level: vimtypes.SharesLevelNormal,
			}
		}
		if v := memReq; !v.IsZero() {
			rsv := MemoryQuantityToMb(v)
			configSpec.MemoryAllocation.Reservation = &rsv
		}
		if v := memLim; !v.IsZero() {
			rsv := MemoryQuantityToMb(v)
			configSpec.MemoryAllocation.Limit = &rsv
		}
	}

	// Override the firmware with the provided firmware type.
	if firmware != "" && configSpec.Firmware != firmware {
		configSpec.Firmware = firmware
	}

	return configSpec
}

// CreateConfigSpecForPlacement creates a ConfigSpec to use for placement. Once CL deploy can accept
// a ConfigSpec, this should largely - or ideally entirely - be folded into CreateConfigSpec() above.
func CreateConfigSpecForPlacement(
	vmCtx context.VirtualMachineContext,
	vmClassSpec *v1alpha1.VirtualMachineClassSpec,
	minFreq uint64,
	storageClassesToIDs map[string]string,
	imageFirmware string,
	vmClassConfigSpec *vimtypes.VirtualMachineConfigSpec) *vimtypes.VirtualMachineConfigSpec {

	configSpec := CreateConfigSpec(vmCtx.VM.Name, vmClassSpec, minFreq, imageFirmware, vmClassConfigSpec)

	// Add a dummy disk for placement: PlaceVmsXCluster expects there to always be at least one disk.
	// Until we're in a position to have the OVF envelope here, add a dummy disk satisfy it.
	configSpec.DeviceChange = append(configSpec.DeviceChange, &vimtypes.VirtualDeviceConfigSpec{
		Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
		FileOperation: vimtypes.VirtualDeviceConfigSpecFileOperationCreate,
		Device: &vimtypes.VirtualDisk{
			CapacityInBytes: 1024 * 1024,
			VirtualDevice: vimtypes.VirtualDevice{
				Key: -42,
				Backing: &vimtypes.VirtualDiskFlatVer2BackingInfo{
					ThinProvisioned: pointer.Bool(true),
				},
			},
		},
		Profile: []vimtypes.BaseVirtualMachineProfileSpec{
			&vimtypes.VirtualMachineDefinedProfileSpec{
				ProfileId: storageClassesToIDs[vmCtx.VM.Spec.StorageClass],
			},
		},
	})

	for _, dev := range CreatePCIDevices(vmClassSpec.Hardware.Devices, nil) {
		configSpec.DeviceChange = append(configSpec.DeviceChange, &vimtypes.VirtualDeviceConfigSpec{
			Operation: vimtypes.VirtualDeviceConfigSpecOperationAdd,
			Device:    dev,
		})
	}

	if lib.IsInstanceStorageFSSEnabled() {
		isVolumes := instancestorage.FilterVolumes(vmCtx.VM)

		for idx, dev := range CreateInstanceStorageDiskDevices(isVolumes) {
			configSpec.DeviceChange = append(configSpec.DeviceChange, &vimtypes.VirtualDeviceConfigSpec{
				Operation:     vimtypes.VirtualDeviceConfigSpecOperationAdd,
				FileOperation: vimtypes.VirtualDeviceConfigSpecFileOperationCreate,
				Device:        dev,
				Profile: []vimtypes.BaseVirtualMachineProfileSpec{
					&vimtypes.VirtualMachineDefinedProfileSpec{
						ProfileId: storageClassesToIDs[isVolumes[idx].PersistentVolumeClaim.InstanceVolumeClaim.StorageClass],
						ProfileData: &vimtypes.VirtualMachineProfileRawData{
							ExtensionKey: "com.vmware.vim.sps",
						},
					},
				},
			})
		}
	}

	// TODO: Add more devices and fields
	//  - boot disks from OVA
	//  - storage profile/class
	//  - PVC volumes
	//  - Network devices (meh for now b/c of wcp constraints)
	//  - anything in ExtraConfig matter here?
	//  - any way to do the cluster modules for anti-affinity?
	//  - whatever else I'm forgetting

	return configSpec
}
