// Copyright (c) 2019-2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha2_test

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"

	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	vmopv1a2 "github.com/vmware-tanzu/vm-operator/api/v1alpha2"
	virtualmachine "github.com/vmware-tanzu/vm-operator/controllers/virtualmachine/v1alpha2"
	ctrlContext "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/lib"
	pkgmgr "github.com/vmware-tanzu/vm-operator/pkg/manager"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	vsphere2 "github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere2"
	"github.com/vmware-tanzu/vm-operator/test/builder"
	mutationv1a1 "github.com/vmware-tanzu/vm-operator/webhooks/virtualmachine/v1alpha1/mutation"
	validationv1a1 "github.com/vmware-tanzu/vm-operator/webhooks/virtualmachine/v1alpha1/validation"
	mutationv1a2 "github.com/vmware-tanzu/vm-operator/webhooks/virtualmachine/v1alpha2/mutation"
	validationv1a2 "github.com/vmware-tanzu/vm-operator/webhooks/virtualmachine/v1alpha2/validation"
)

var suite = builder.NewTestSuiteWithOptions(
	builder.TestSuiteOptions{
		InitProviderFn: func(ctx *ctrlContext.ControllerManagerContext, mgr ctrlmgr.Manager) error {
			vmProviderName := fmt.Sprintf("%s/%s/vmProvider", ctx.Namespace, ctx.Name)
			recorder := record.New(mgr.GetEventRecorderFor(vmProviderName))
			ctx.VMProviderA2 = vsphere2.NewVSphereVMProviderFromClient(mgr.GetClient(), recorder)
			return nil
		},
		FeatureStates: map[string]bool{
			lib.WcpFaultDomainsFSS:   false,
			lib.VMServiceV1Alpha2FSS: true,
			lib.NamespacedVMClassFSS: true,
			lib.VMImageRegistryFSS:   true,
		},
		IntegrationTests: builder.TestSuiteIntegrationTestOptions{
			VSphere: &builder.VSphereOptions{
				VCSim: builder.VCSimOptions{
					Enabled: true,
				},
			},
			Controllers: []pkgmgr.AddToManagerFunc{virtualmachine.AddToManager},
			ConversionWebhooks: []builder.TestSuiteConversionWebhookOptions{
				{
					Name: "virtualmachines.vmoperator.vmware.com",
					AddToManagerFn: []func(ctrlmgr.Manager) error{
						(&vmopv1.VirtualMachine{}).SetupWebhookWithManager,
						(&vmopv1a2.VirtualMachine{}).SetupWebhookWithManager,
					},
				},
			},
			MutationWebhooks: []builder.TestSuiteMutationWebhookOptions{
				{
					Name:           "default.mutating.virtualmachine.v1alpha1.vmoperator.vmware.com",
					AddToManagerFn: mutationv1a1.AddToManager,
				},
				{
					Name:           "default.mutating.virtualmachine.v1alpha2.vmoperator.vmware.com",
					AddToManagerFn: mutationv1a2.AddToManager,
				},
			},
			ValidationWebhooks: []builder.TestSuiteValidationWebhookOptions{
				{
					Name:           "default.validating.virtualmachine.v1alpha1.vmoperator.vmware.com",
					AddToManagerFn: validationv1a1.AddToManager,
				},
				{
					Name:           "default.validating.virtualmachine.v1alpha2.vmoperator.vmware.com",
					AddToManagerFn: validationv1a2.AddToManager,
				},
			},
		},
	})

func TestVirtualMachine(t *testing.T) {
	suite.Register(t, "VirtualMachine controller suite", intgTests, unitTests)
}

var _ = BeforeSuite(suite.BeforeSuite)

var _ = AfterSuite(suite.AfterSuite)
