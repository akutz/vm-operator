// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package xoat

import (
	"context"
	"sync"
)

type channelMap struct {
	sync.Mutex
	data map[any]chan struct{}
}

// ExecuteOneAtATime executes the provided function for the provided ctxKey and
// mapKey type.
// If concurrent calls are received for the same ctxKey and mapKey, only one
// call is executed and others are silently ignored.
// True is returned if the function was scheduled to be executed, otherwise
// false is returned.
func ExecuteOneAtATime(
	ctx context.Context,
	ctxKey, mapKey any,
	fn func()) bool {

	if fn == nil {
		panic("fn is nil")
	}

	chanWait := createOrGetChan(ctx, ctxKey, mapKey)

	select {
	case chanWait <- struct{}{}:
		go func() {
			// Execute the function.
			fn()

			// Remove the item on the wait channel, indicating it is fine for
			// others to send on this channel again.
			<-chanWait
		}()

		return true
	case <-ctx.Done():
		// The context is has timed out or been cancelled; drop request.
	default:
		// The wait channel could not immediately receive; drop request.
	}

	return false
}

// createOrGetChan creates or gets the channel for the specified ctxKey and
// mapKey.
func createOrGetChan(ctx context.Context, ctxKey, mapKey any) chan struct{} {

	if ctx == nil {
		panic("ctx is nil")
	}
	if ctxKey == nil {
		panic("ctxKey is nil")
	}
	if mapKey == nil {
		panic("mapKey is nil")
	}
	obj := ctx.Value(ctxKey)
	if obj == nil {
		panic("map is missing from context")
	}

	m := ctx.Value(ctxKey).(*channelMap)
	m.Lock()
	defer m.Unlock()

	if c, ok := m.data[mapKey]; ok {
		return c
	}

	// The channel *must* be buffered or else the Execute function will never
	// work as expected.
	c := make(chan struct{}, 1)
	m.data[mapKey] = c

	return c
}

// DeleteChan removes the channel for the specified ctxKey and mapKey.
func DeleteChan(ctx context.Context, ctxKey, mapKey any) {

	if ctx == nil {
		return
	}

	if m, ok := ctx.Value(ctxKey).(*channelMap); ok {
		m.Lock()
		defer m.Unlock()
		delete(m.data, mapKey)
	}
}

// DoesChanExist returns true if a channel exists for the provided ctxKey and
// mapKey; otherwise false is returned.
//
// Please note, this is ONLY intended for testing and should *not* be used in
// production.
func DoesChanExist(ctx context.Context, ctxKey, mapKey any) bool {

	if ctx == nil {
		panic("ctx is nil")
	}
	if ctxKey == nil {
		panic("ctxKey is nil")
	}
	if mapKey == nil {
		panic("mapKey is nil")
	}
	obj := ctx.Value(ctxKey)
	if obj == nil {
		panic("map is missing from context")
	}

	m := ctx.Value(ctxKey).(*channelMap)
	m.Lock()
	defer m.Unlock()

	_, ok := m.data[mapKey]

	return ok
}

// NewContext returns a new context with a new channels map.
func NewContext(ctxKey any) context.Context {
	return WithContext(context.Background(), ctxKey)
}

// JoinContext returns a new context that contains a reference to the channels
// map from the specified context.
// This function panics if the provided context does not contain a channels map.
// This function is thread-safe.
func JoinContext(
	parent context.Context,
	contextWithMap context.Context,
	ctxKey any) context.Context {

	if parent == nil {
		panic("parent context is nil")
	}
	if contextWithMap == nil {
		panic("contextWithMap is nil")
	}
	if ctxKey == nil {
		panic("ctxKey is nil")
	}
	obj := contextWithMap.Value(ctxKey)
	if obj == nil {
		panic("map is missing from context")
	}
	return context.WithValue(parent, ctxKey, obj.(*channelMap))
}

// WithContext returns a new context with a new channels map, with the provided
// context as the parent.
func WithContext(
	parent context.Context,
	ctxKey any) context.Context {

	if parent == nil {
		panic("parent context is nil")
	}
	return context.WithValue(
		parent,
		ctxKey,
		&channelMap{data: map[any]chan struct{}{}})
}
