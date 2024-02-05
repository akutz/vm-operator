// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package watcher_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestWatcherVmChangeVersion(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "vSphere Watcher Test Suite")
}
