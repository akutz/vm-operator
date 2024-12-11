// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package library

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"path"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	vimtypes "github.com/vmware/govmomi/vim25/types"
)

type moRef = vimtypes.ManagedObjectReference

// CacheStorageURIs copies the disk(s) from srcDiskURIs to dstDir and returns
// the path(s) to the copied disk(s).
func CacheStorageURIs(
	ctx context.Context,
	vimClient *vim25.Client,
	dstDatacenterRef, srcDatacenterRef, dstDatastoreRef moRef,
	dstDatastoreName, dstDir string, srcDiskURIs ...string) ([]string, error) {

	dstDatacenter := object.NewDatacenter(vimClient, dstDatacenterRef)
	srcDatacenter := object.NewDatacenter(vimClient, srcDatacenterRef)

	var dstStorageURIs []string
	diskMgr := newVirtualDiskManager(vimClient)
	fileManager := object.NewFileManager(vimClient)

	for i := range srcDiskURIs {
		dstFilePath, err := copyDisk(
			ctx,
			diskMgr,
			fileManager,
			dstDir,
			srcDiskURIs[i],
			dstDatacenter,
			srcDatacenter)
		if err != nil {
			return nil, err
		}
		dstStorageURIs = append(dstStorageURIs, dstFilePath)
	}

	return dstStorageURIs, nil
}

func copyDisk(
	ctx context.Context,
	diskMgr *virtualDiskManager,
	fileMgr *object.FileManager,
	dstDir, srcFilePath string,
	dstDatacenter, srcDatacenter *object.Datacenter) (string, error) {

	srcFileName := path.Base(srcFilePath)
	dstFileName := GetCachedFileNameForVMDK(srcFileName) + ".vmdk"
	dstFilePath := path.Join(dstDir, dstFileName)

	_, queryDiskErr := diskMgr.QueryVirtualDiskUuid(
		ctx,
		dstFilePath,
		dstDatacenter)
	if queryDiskErr == nil {
		// Disk exists, return the path to it.
		return dstFilePath, nil
	}
	if !fault.Is(queryDiskErr, &vimtypes.FileNotFound{}) {
		return "", fmt.Errorf("failed to query disk uuid: %w", queryDiskErr)
	}

	// Create the VM folder.
	if err := fileMgr.MakeDirectory(
		ctx,
		dstDir,
		dstDatacenter,
		true); err != nil {

		return "", fmt.Errorf("failed to create folder %q: %w", dstDir, err)
	}

	// The base disk does not exist, create it.
	copyDiskTask, err := diskMgr.CopyVirtualDisk(
		ctx,
		srcFilePath,
		srcDatacenter,
		dstFilePath,
		dstDatacenter,
		&vimtypes.FileBackedVirtualDiskSpec{
			VirtualDiskSpec: vimtypes.VirtualDiskSpec{
				AdapterType: string(vimtypes.VirtualDiskAdapterTypeLsiLogic),
				DiskType:    string(vimtypes.VirtualDiskTypeThin),
			},
		},
		false)
	if err != nil {
		return "", fmt.Errorf("failed to call copy disk: %w", err)
	}
	if err := copyDiskTask.Wait(ctx); err != nil {
		return "", fmt.Errorf("failed to wait for copy disk: %w", err)
	}

	return dstFilePath, nil
}

// GetCachedDirForLibraryItem returns the cache directory for a given library on
// a specified datastore.
func GetCachedDirForLibraryItem(
	dstDatastoreName,
	itemUUID,
	contentVersion string) string {

	return fmt.Sprintf("[%s] .contentlib-cache/%s/%s",
		dstDatastoreName,
		itemUUID,
		contentVersion)
}

// GetCachedFileNameForVMDK returns the first 17 characters of a SHA-1 sum of
// a VMDK file name and extension, ex. my-disk.vmdk.
func GetCachedFileNameForVMDK(s string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))[0:17]
}

type virtualDiskManager struct {
	*object.VirtualDiskManager
}

func newVirtualDiskManager(c *vim25.Client) *virtualDiskManager {
	m := virtualDiskManager{
		VirtualDiskManager: object.NewVirtualDiskManager(c),
	}
	return &m
}

func (m virtualDiskManager) CopyVirtualDisk(
	ctx context.Context,
	sourceName string, sourceDatacenter *object.Datacenter,
	destName string, destDatacenter *object.Datacenter,
	destSpec vimtypes.BaseVirtualDiskSpec, force bool) (*object.Task, error) {

	req := vimtypes.CopyVirtualDisk_Task{
		This:       m.Reference(),
		SourceName: sourceName,
		DestName:   destName,
		DestSpec:   destSpec,
		Force:      vimtypes.NewBool(force),
	}

	if sourceDatacenter != nil {
		ref := sourceDatacenter.Reference()
		req.SourceDatacenter = &ref
	}

	if destDatacenter != nil {
		ref := destDatacenter.Reference()
		req.DestDatacenter = &ref
	}

	res, err := methods.CopyVirtualDisk_Task(ctx, m.Client(), &req)
	if err != nil {
		return nil, err
	}

	return object.NewTask(m.Client(), res.Returnval), nil
}
