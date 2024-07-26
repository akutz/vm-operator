// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package storagepolicyquota

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	spqv1 "github.com/vmware-tanzu/vm-operator/external/storage-policy-quota/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/patch"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
	spqutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube/spq"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
)

const (
	finalizerName = "vmoperator.vmware.com/storagepolicyquota"
)

// AddToManager adds this package's controller to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr manager.Manager) error {
	var (
		controlledType     = &spqv1.StoragePolicyQuota{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, controllerNameShort)
	)

	r := NewReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("controllers").WithName(controlledTypeName),
		record.New(mgr.GetEventRecorderFor(controllerNameLong)),
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(controlledType).
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

// Reconciler reconciles a StoragePolicyQuota object.
type Reconciler struct {
	client.Client
	Context  context.Context
	Logger   logr.Logger
	Recorder record.Recorder
}

//
// Please note, the delete permissions are required on storagepolicyquotas
// because it is set as the ControllerOwnerReference for the created
// StoragePolicyUsage resource. If a controller owner reference does not have
// delete on the owner, then a 422 (unprocessable entity) is returned.
//

// +kubebuilder:rbac:groups=cns.vmware.com,resources=storagepolicyquotas,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=cns.vmware.com,resources=storagepolicyquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cns.vmware.com,resources=storagepolicyusages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cns.vmware.com,resources=storagepolicyusages/status,verbs=get;update;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx = pkgcfg.JoinContext(ctx, r.Context)

	var obj spqv1.StoragePolicyQuota
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger := ctrl.Log.WithName(spqutil.StoragePolicyQuotaKind).WithValues(
		"name", req.NamespacedName)

	// Ensure the GVK for the object is synced back into the object since
	// the object's APIVersion and Kind fields may be used later.
	kubeutil.MustSyncGVKToObject(&obj, r.Scheme())

	patchHelper, err := patch.NewHelper(&obj, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper for %s: %w", req, err)
	}

	defer func() {
		if err := patchHelper.Patch(ctx, &obj); err != nil {
			if reterr == nil {
				reterr = err
			}
			logger.Error(err, "patch failed")
		}
	}()

	if !obj.DeletionTimestamp.IsZero() {
		if err := r.ReconcileDelete(ctx, logger, &obj); err != nil {
			logger.Error(err, "Failed to ReconcileDelete StoragePolicyQuota")
			return ctrl.Result{}, err
		}
	}

	if err := r.ReconcileNormal(ctx, logger, &obj); err != nil {
		logger.Error(err, "Failed to ReconcileNormal StoragePolicyQuota")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) ReconcileDelete(
	ctx context.Context,
	logger logr.Logger,
	src *spqv1.StoragePolicyQuota) error {

	if !ctrlutil.ContainsFinalizer(src, finalizerName) {
		return nil
	}

	if err := spqutil.DeleteChanForStoragePolicy(
		ctx,
		r.Client,
		src.Namespace,
		src.Spec.StoragePolicyId); err != nil {

		return err
	}

	ctrlutil.RemoveFinalizer(src, finalizerName)

	return nil
}

func (r *Reconciler) ReconcileNormal(
	ctx context.Context,
	logger logr.Logger,
	src *spqv1.StoragePolicyQuota) error {

	// Add the finalizer and return early to ensure no work is done until
	// the finalizer is added. The act of patching the finalizer will cause this
	// object to get requeued.
	if ctrlutil.AddFinalizer(src, finalizerName) {
		return nil
	}

	// Get the list of storage classes for the provided policy ID.
	objs, err := spqutil.GetStorageClassesForPolicy(
		ctx,
		r.Client,
		src.Namespace,
		src.Spec.StoragePolicyId)
	if err != nil {
		return err
	}

	// Create the StoragePolicyUsage resources.
	for i := range objs {
		dst := spqv1.StoragePolicyUsage{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: src.Namespace,
				Name:      spqutil.StoragePolicyUsageName(objs[i].Name),
			},
		}

		fn := func() error {
			if err := ctrlutil.SetControllerReference(
				src, &dst, r.Scheme()); err != nil {

				return err
			}

			dst.Spec.StorageClassName = objs[i].Name
			dst.Spec.StoragePolicyId = src.Spec.StoragePolicyId
			dst.Spec.ResourceAPIgroup = ptr.To(spqv1.GroupVersion.Group)
			dst.Spec.ResourceKind = spqutil.StoragePolicyQuotaKind
			dst.Spec.ResourceExtensionName = spqutil.StoragePolicyQuotaExtensionName

			return nil
		}

		if _, err := ctrlutil.CreateOrPatch(
			ctx,
			r.Client,
			&dst,
			fn); err != nil {

			return err
		}
	}

	return err
}
