// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// Hub marks VirtualMachine as a conversion hub.
func (*VirtualMachine) Hub() {}

// Hub marks VirtualMachineList as a conversion hub.
func (*VirtualMachineList) Hub() {}
