// © Broadcom. All Rights Reserved.
// The term “Broadcom” refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"context"
	"fmt"
	"sync"
	"time"

	expcache "github.com/go-pkgz/expirable-cache/v3"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"
)

type nsxOpaqueBacking struct {
	vimtypes.ManagedObjectReference // Won't be set.
	logicalSwitchUID                string
}

const (
	// dvpgCacheMaxKeys is the maximum number of CCR to DVPG mappings to cache.
	dvpgCacheMaxKeys = 10000

	// dvpgCacheTTL is the expiry time for items in the cache.
	dvpgCacheTTL = time.Hour * 24

	// stringSizeBytes is the number of bytes allocated for a string data
	// structure.
	stringSizeBytes = 16
)

var (
	// DVPGCache is an LRU cache used to cache CCR-to-DVPG mappings.
	DVPGCache expcache.Cache[string, map[string]vimtypes.ManagedObjectReference]
)

func init() {
	DVPGCache = expcache.NewCache[string, map[string]vimtypes.ManagedObjectReference]().
		WithLRU().
		WithMaxKeys(dvpgCacheMaxKeys).
		WithTTL(dvpgCacheTTL)

	sync.OnceFunc(func() {
		// Every 30m print the cache statistics.
		ticker := time.NewTicker(30 * time.Minute)
		logger := ctrl.Log.WithName("nsxt-dvpg-cache-stats")
		go func() {
			for range ticker.C {
				kvp := DVGPCacheGetStats()
				logger.Info("NSXT DVPG cache stats", kvp...)
			}
		}()
	})
}

// DVGPCacheGetStats returns the key/value pairs required to log the cache's
// stats.
// This is an expensive operation as it locks the cache to get the base stats
// and then again, per object in the cache, to calculate the total size of the
// cache.
func DVGPCacheGetStats() []any {
	keyValPairs := []any{}

	s := DVPGCache.Stat()
	keyValPairs = append(
		keyValPairs,
		"added", s.Added,
		"evicted", s.Evicted,
		"hits", s.Hits,
		"misses", s.Misses)

	var (
		size uint64
		keys = DVPGCache.Keys()
	)

	keyValPairs = append(keyValPairs, "items", len(keys))

	for _, k := range keys {
		if v, ok := DVPGCache.Peek(k); ok {
			size += stringSizeBytes + uint64(len(k))
			for k, v := range v {
				size += stringSizeBytes + uint64(len(k))
				size += stringSizeBytes + uint64(len(v.ServerGUID))
				size += stringSizeBytes + uint64(len(v.Type))
				size += stringSizeBytes + uint64(len(v.Value))
			}
		}
	}
	keyValPairs = append(keyValPairs, "bytes", size)

	return keyValPairs
}

// NSX-T/VPC provides us with the logical switch UID, not the opaque network
// MoRef, so implement a simpler version of govmomi's OpaqueNetwork here.
var _ object.NetworkReference = &nsxOpaqueBacking{}

func newNSXOpaqueNetwork(lsUID string) *nsxOpaqueBacking {
	return &nsxOpaqueBacking{
		logicalSwitchUID: lsUID,
	}
}

func (n nsxOpaqueBacking) GetInventoryPath() string {
	return ""
}

// EthernetCardBackingInfo returns the VirtualDeviceBackingInfo for this Network.
func (n nsxOpaqueBacking) EthernetCardBackingInfo(ctx context.Context) (vimtypes.BaseVirtualDeviceBackingInfo, error) {
	summary, err := n.Summary(ctx)
	if err != nil {
		return nil, err
	}

	backing := &vimtypes.VirtualEthernetCardOpaqueNetworkBackingInfo{
		OpaqueNetworkId:   summary.OpaqueNetworkId,
		OpaqueNetworkType: summary.OpaqueNetworkType,
	}

	return backing, nil
}

func (n nsxOpaqueBacking) Summary(_ context.Context) (*vimtypes.OpaqueNetworkSummary, error) {
	return &vimtypes.OpaqueNetworkSummary{
		OpaqueNetworkId:   n.logicalSwitchUID,
		OpaqueNetworkType: "nsx.LogicalSwitch",
	}, nil
}

func searchNsxtNetworkReference(
	ctx context.Context,
	ccr *object.ClusterComputeResource,
	networkID string) (object.NetworkReference, error) {

	key := ccr.Reference().Value

	//if c, ok := uuidToDVPGCache.Load(key); ok {
	if c, ok := DVPGCache.Get(key); ok {
		if dvpg, ok := c[networkID]; ok {
			return object.NewDistributedVirtualPortgroup(ccr.Client(), dvpg), nil
		}
	}

	// On either miss - CCR or UUID not found - try to refresh the DVPGs for the CCR,
	// and always store the latest results in the cache. We could  CompareAndSwap()
	// instead but let's have the latest win.
	uuidsToDPVG, err := getDVPGsForCCR(ctx, ccr)
	if err != nil {
		return nil, err
	}
	DVPGCache.Add(key, uuidsToDPVG)

	if dvpg, ok := uuidsToDPVG[networkID]; ok {
		return object.NewDistributedVirtualPortgroup(ccr.Client(), dvpg), nil
	}

	return nil, fmt.Errorf("no DVPG with NSX network ID %q found", networkID)
}

func getDVPGsForCCR(
	ctx context.Context,
	ccr *object.ClusterComputeResource) (map[string]vimtypes.ManagedObjectReference, error) {

	var obj mo.ClusterComputeResource
	if err := ccr.Properties(ctx, ccr.Reference(), []string{"network"}, &obj); err != nil {
		return nil, err
	}

	var dvpgsMoRefs []vimtypes.ManagedObjectReference
	for _, n := range obj.Network {
		if n.Type == "DistributedVirtualPortgroup" {
			dvpgsMoRefs = append(dvpgsMoRefs, n.Reference())
		}
	}

	if len(dvpgsMoRefs) == 0 {
		return nil, nil
	}

	var dvpgs []mo.DistributedVirtualPortgroup
	err := property.DefaultCollector(ccr.Client()).Retrieve(ctx, dvpgsMoRefs, []string{"config.logicalSwitchUuid"}, &dvpgs)
	if err != nil {
		return nil, err
	}

	uuidToDPVG := make(map[string]vimtypes.ManagedObjectReference, len(dvpgs))
	for _, dvpg := range dvpgs {
		if uuid := dvpg.Config.LogicalSwitchUuid; uuid != "" {
			uuidToDPVG[uuid] = dvpg.Reference()
		}
	}

	return uuidToDPVG, nil
}
