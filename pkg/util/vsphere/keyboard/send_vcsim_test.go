// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package keyboard_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vmware/govmomi/object"

	"github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/keyboard"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

func sendVCSimTests() {
	Describe("SendCommands against a real object.VirtualMachine", func() {
		var (
			ctx *builder.TestContextForVCSim
			obj *object.VirtualMachine
		)

		BeforeEach(func() {
			ctx = suite.NewTestContextForVCSim(builder.VCSimTestConfig{})
			vmList, err := ctx.Finder.VirtualMachineList(ctx, "*")
			Expect(err).ToNot(HaveOccurred())
			Expect(len(vmList)).To(BeNumerically(">", 0))
			obj = object.NewVirtualMachine(ctx.VCClient.Client, vmList[0].Reference())
		})

		AfterEach(func() {
			ctx.AfterEach()
			ctx = nil
		})

		It("wraps the vcsim MethodNotFound fault as an error", func() {
			// vcsim's simulator does not implement PutUsbScanCodes, so this
			// only proves SendCommands wires a real *object.VirtualMachine
			// through to the vSphere API correctly -- it cannot verify
			// keystroke delivery, which requires a real vCenter/ESXi host.
			tokens, err := keyboard.ParseCommands([]string{"<esc>"}, nil)
			Expect(err).ToNot(HaveOccurred())

			err = keyboard.SendCommands(ctx, obj, tokens)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to send USB scan codes"))
		})
	})
}
