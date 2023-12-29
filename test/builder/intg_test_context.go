// Copyright (c) 2019 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package builder

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vmware-tanzu/vm-operator/pkg/lib"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IntegrationTestContext is used for integration testing. Each
// IntegrationTestContext contains one separate namespace.
type IntegrationTestContext struct {
	context.Context
	Client                   client.Client
	Namespace                string
	PodNamespace             string
	DefaultVMClass           string
	DefaultVMImageForLinux   string
	DefaultVMImageForWindows string
	DefaultStorageClass      string

	afterFn func() error

	vsphereConfig *vsphereConfig
}

// AfterEach should be invoked by ginkgo to destroy the test namespace.
func (ctx *IntegrationTestContext) AfterEach() {
	By("Destroying temporary namespace", func() {
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: ctx.Namespace,
			},
		}
		Expect(ctx.Client.Delete(ctx, namespace)).To(Succeed())
	})

	if ctx.afterFn != nil {
		Expect(ctx.afterFn()).To(Succeed())
	}
}

// NewIntegrationTestContext should be invoked by ginkgo.BeforeEach
//
// This function creates a namespace with a random name to separate integration
// test cases
//
// This function returns a TestSuite context
// The resources created by this function may be cleaned up by calling AfterEach
// with the IntegrationTestContext returned by this function.
func (s *TestSuite) NewIntegrationTestContext() *IntegrationTestContext {
	ctx := &IntegrationTestContext{
		Context:      context.Background(),
		Client:       s.integration.client,
		PodNamespace: s.integration.manager.GetContext().Namespace,
	}

	if s.isVSphereEnabled() {
		ctx.vsphereConfig = s.integration.vsphere
		ctx.DefaultStorageClass = s.integration.vsphere.opts.StorageClassName
		ctx.DefaultVMClass = "best-effort-small"

		if s.fssMap[lib.VMImageRegistryFSS] {
			ctx.DefaultVMImageForLinux = "vmi-0123456789"
			ctx.DefaultVMImageForWindows = "vmi-abcdefghij"
		} else {
			ctx.DefaultVMImageForLinux = "linux"
			ctx.DefaultVMImageForWindows = "windows"
		}
	}

	By("Creating a temporary namespace", func() {
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "workload-",
			},
		}
		Expect(ctx.Client.Create(s, namespace)).To(Succeed())
		ctx.Namespace = namespace.Name

		if s.isVSphereEnabled() {
			_, _, err := s.integration.vsphere.createWorkloadNamespace(
				ctx, ctx.Client, namespace.Name)
			Expect(err).ToNot(HaveOccurred())

			ctx.afterFn = func() error {
				return ctx.vsphereConfig.destroyWorkloadNamespace(
					ctx, ctx.Client, ctx.Namespace)
			}
		}
	})

	return ctx
}
