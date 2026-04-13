// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	vimv1 "github.com/vmware-tanzu/vm-operator/external/vim/api/v1alpha1"
)

// ConvertFunc is the signature for a function that is used to convert between
// a VIM CRD type and a VIM VMODL type.
type ConvertFunc func(dstObj, srcObj any) error

var (
	vim2CRDFuncs = map[string]ConvertFunc{}
	crd2VIMFuncs = map[string]ConvertFunc{}
)

func ConvertVIMToCRD(dst, src any) error {
	var _ vimv1.VirtualMachineConfigPolicy
	panic("Not Implemented")
}

func init() {
	vim2CRDFuncs["VirtualMachineConfigInfoSpec"] = convertVIMToCRD_ConfigInfoSpec
}

func convertVIMToCRD_ConfigInfoSpec(dstObj, srcObj any) error {
	panic("Not Implemented")
}
