// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"

	imgregv1a1 "github.com/vmware-tanzu/image-registry-operator-api/api/v1alpha1"
)

// SyncLibraryItem issues a sync call to the provided library item.
func SyncLibraryItem(
	ctx context.Context,
	client *rest.Client,
	item imgregv1a1.ContentLibraryItem) error {

	if item.Status.Cached && item.Status.SizeInBytes.Size() > 0 {
		return nil
	}

	var (
		id     = string(item.Spec.UUID)
		mgr    = library.NewManager(client)
		logger = logr.FromContextOrDiscard(ctx)
	)

	// A file from a library item that belongs to a subscribed library may not
	// be fully available. Sync the file to ensure it is present.
	logger.Info("Syncing content library item", "libraryItemID", id)
	libItem, err := mgr.GetLibraryItem(ctx, id)
	if err != nil {
		return fmt.Errorf(
			"error getting library item %s to sync: %w", id, err)
	}
	if err := mgr.SyncLibraryItem(ctx, libItem, true); err != nil {
		return fmt.Errorf(
			"error syncing library item %s: %w", id, err)
	}
	return nil
}
