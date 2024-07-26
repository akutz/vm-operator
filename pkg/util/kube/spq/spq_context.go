// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package spq

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/vm-operator/pkg/util/xoat"
)

type contextKeyType uint8

var contextKeyValue contextKeyType = 0

// ExecuteForStorageClass executes the provided function for the storage class
// in the given namespace.
// If concurrent calls are received for the same namespace / storage class, only
// one call is executed and others are silently ignored.
// A channel is returned on on which a true value is sent if the function was
// scheduled; otherwise a false value is sent.
func ExecuteForStorageClass(
	ctx context.Context,
	namespace, name string,
	fn func()) bool {

	if namespace == "" {
		panic("namespace is empty")
	}
	if name == "" {
		panic("name is empty")
	}

	return xoat.ExecuteOneAtATime(
		ctx,
		contextKeyValue,
		mapKey(namespace, name),
		fn)
}

// DeleteChanForStoragePolicy removes the channels for the storage classes
// linked to the provided storage policy in a given namespace.
func DeleteChanForStoragePolicy(
	ctx context.Context,
	k8sClient client.Client,
	namespace, id string) error {

	if ctx == nil {
		return nil
	}

	objs, err := GetStorageClassesForPolicy(ctx, k8sClient, namespace, id)
	if err != nil {
		return err
	}

	for i := range objs {
		xoat.DeleteChan(ctx, contextKeyValue, mapKey(namespace, objs[i].Name))
	}

	return nil
}

// DoesChanExistForStorageClass returns a flag indicating whether a channel
// already exists for the provided storage class in the given namespace.
//
// Please note, this is ONLY intended for testing and should *not* be used in
// production.
func DoesChanExistForStorageClass(
	ctx context.Context,
	namespace, name string) bool {

	if namespace == "" {
		panic("namespace is empty")
	}
	if name == "" {
		panic("name is empty")
	}

	return xoat.DoesChanExist(ctx, contextKeyValue, mapKey(namespace, name))
}

// NewContext returns a new context with a new channels map.
func NewContext() context.Context {
	return xoat.NewContext(contextKeyValue)
}

// JoinContext returns a new context that contains a reference to the channels
// map from the specified context.
// This function panics if the provided context does not contain a channels map.
// This function is thread-safe.
func JoinContext(
	parent context.Context,
	contextWithMap context.Context) context.Context {

	return xoat.JoinContext(parent, contextWithMap, contextKeyValue)
}

// WithContext returns a new context with a new channels map, with the provided
// context as the parent.
func WithContext(parent context.Context) context.Context {
	return xoat.WithContext(parent, contextKeyValue)
}

func mapKey(namespace, name string) string {
	return namespace + "/" + name
}
