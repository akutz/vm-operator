// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package storagepolicyusage

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha3"
	spqv1alpha1 "github.com/vmware-tanzu/vm-operator/external/storage-policy-quota/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/patch"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
)

// AddToManager adds this package's controller to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr manager.Manager) error {
	var (
		controllerName      = "storagepolicyusage"
		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controllerName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, controllerNameShort)
	)

	r := NewReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("controllers").WithName(controllerName),
		record.New(mgr.GetEventRecorderFor(controllerNameLong)),
	)

	_ = spqv1alpha1.AddToScheme

	return ctrl.NewControllerManagedBy(mgr).
		For(nil).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func NewReconciler(
	ctx context.Context,
	client client.Client,
	logger logr.Logger,
	recorder record.Recorder) *Reconciler {

	return &Reconciler{
		Context:  ctx,
		Client:   client,
		Logger:   logger,
		Recorder: recorder,
	}
}

// Reconciler reconciles a VirtualMachineClass object.
type Reconciler struct {
	client.Client
	Context  context.Context
	Logger   logr.Logger
	Recorder record.Recorder
}

// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclasses/status,verbs=get;update;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx = pkgcfg.JoinContext(ctx, r.Context)

	vmClass := &vmopv1.VirtualMachineClass{}
	if err := r.Get(ctx, req.NamespacedName, vmClass); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	vmClassCtx := &pkgctx.VirtualMachineClassContext{
		Context: ctx,
		Logger:  ctrl.Log.WithName("VirtualMachineClass").WithValues("name", req.Name),
		VMClass: vmClass,
	}

	patchHelper, err := patch.NewHelper(vmClass, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper for %s: %w", vmClassCtx, err)
	}
	defer func() {
		if err := patchHelper.Patch(ctx, vmClass); err != nil {
			if reterr == nil {
				reterr = err
			}
			vmClassCtx.Logger.Error(err, "patch failed")
		}
	}()

	if !vmClass.DeletionTimestamp.IsZero() {
		// Noop.
		return ctrl.Result{}, nil
	}

	if err := r.ReconcileNormal(vmClassCtx); err != nil {
		vmClassCtx.Logger.Error(err, "Failed to reconcile VirtualMachineClass")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) ReconcileNormal(vmClassCtx *pkgctx.VirtualMachineClassContext) error {
	return nil
}
