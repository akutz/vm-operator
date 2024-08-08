// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package pusher

import (
	goctx "context"

	"github.com/vmware/govmomi/vim25/types"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha2"
	"github.com/vmware-tanzu/vm-operator/pkg/context"
	vsphereclient "github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/client"
	"github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/watcher"
)

// AddToManager adds this package's runnable to the provided manager.
func AddToManager(
	ctx *context.ControllerManagerContext,
	mgr manager.Manager) error {

	p := &pusher{}

	if err := mgr.Add(p); err != nil {
		return err
	}

	return nil
}

type pusher struct {
	client.Client
}

func New(
	ctx goctx.Context,
	client client.Client) manager.LeaderElectionRunnable {

	return &pusher{
		Client: client,
	}
}

func (p *pusher) NeedLeaderElection() bool {
	return true
}

func (p *pusher) Start(ctx goctx.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := p.waitForChanges(ctx); err != nil {
			// If waitForChanges failed because of an invalid login or auth
			// error, then do not treat the error as fatal. This allows the
			// loop to run again, kicking off another watcher with what should
			// be updated credentials.
			if !vsphereclient.IsInvalidLogin(err) &&
				!vsphereclient.IsNotAuthenticatedError(err) {

				return err
			}
		}
	}
}

var emptyResult watcher.Result

func (p *pusher) waitForChanges(ctx goctx.Context) error {
	chanResult, chanErr := watcher.Start(
		ctx,
		nil,
		types.ManagedObjectReference{},
		types.ManagedObjectReference{})

	for {
		select {
		case r := <-chanResult:
			if r == emptyResult {
				return nil
			}

			// TODO Validate the namespace exists on this K8s cluster.

			if c := context.GetChannelSource(ctx); c != nil {
				go func() {
					c <- event.GenericEvent{
						Object: &vmopv1.VirtualMachine{
							ObjectMeta: v1.ObjectMeta{
								Namespace: r.Namespace,
								Name:      r.VmName,
							},
						},
					}
				}()
			}
		case err := <-chanErr:
			if err == nil {
				return nil
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
