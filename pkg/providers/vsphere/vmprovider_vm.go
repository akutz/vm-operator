// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"path"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/go-logr/logr"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/pbm"
	pbmtypes "github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrorsutil "k8s.io/apimachinery/pkg/util/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	imgregv1a1 "github.com/vmware-tanzu/image-registry-operator-api/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha4"
	"github.com/vmware-tanzu/vm-operator/api/v1alpha4/common"
	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	pkgcnd "github.com/vmware-tanzu/vm-operator/pkg/conditions"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgconst "github.com/vmware-tanzu/vm-operator/pkg/constants"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	ctxop "github.com/vmware-tanzu/vm-operator/pkg/context/operation"
	pkgerr "github.com/vmware-tanzu/vm-operator/pkg/errors"
	"github.com/vmware-tanzu/vm-operator/pkg/providers"
	vcclient "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/client"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/clustermodules"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/network"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/placement"
	res "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/resources"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/session"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/storage"
	upgradevm "github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/upgrade/virtualmachine"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/vcenter"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/virtualmachine"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/vmlifecycle"
	"github.com/vmware-tanzu/vm-operator/pkg/topology"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
	"github.com/vmware-tanzu/vm-operator/pkg/util/kube/cource"
	vmopv1util "github.com/vmware-tanzu/vm-operator/pkg/util/vmopv1"
	vmutil "github.com/vmware-tanzu/vm-operator/pkg/util/vsphere/vm"
	vmconfcrypto "github.com/vmware-tanzu/vm-operator/pkg/vmconfig/crypto"
	vmconfdiskpromo "github.com/vmware-tanzu/vm-operator/pkg/vmconfig/diskpromo"
)

var (
	ErrSetPowerState          = res.ErrSetPowerState
	ErrUpgradeSchema          = upgradevm.ErrUpgradeSchema
	ErrBackup                 = virtualmachine.ErrBackingUp
	ErrBootstrapReconfigure   = vmlifecycle.ErrBootstrapReconfigure
	ErrBootstrapCustomize     = vmlifecycle.ErrBootstrapCustomize
	ErrReconfigure            = session.ErrReconfigure
	ErrRestart                = pkgerr.NoRequeueNoErr("restarted vm")
	ErrUpgradeHardwareVersion = session.ErrUpgradeHardwareVersion
	ErrIsPaused               = pkgerr.NoRequeueNoErr("is paused")
	ErrHasTask                = pkgerr.NoRequeueNoErr("has outstanding task")
	ErrPromoteDisks           = vmconfdiskpromo.ErrPromoteDisks
	ErrCreate                 = pkgerr.NoRequeueNoErr("created vm")
	ErrUpdate                 = pkgerr.NoRequeueNoErr("updated vm")
)

// VMCreateArgs contains the arguments needed to create a VM on VC.
type VMCreateArgs struct {
	vmlifecycle.CreateArgs
	vmlifecycle.BootstrapData

	VMClass        vmopv1.VirtualMachineClass
	ResourcePolicy *vmopv1.VirtualMachineSetResourcePolicy
	ImageObj       ctrlclient.Object
	ImageSpec      vmopv1.VirtualMachineImageSpec
	ImageStatus    vmopv1.VirtualMachineImageStatus

	Storage               storage.VMStorageData
	HasInstanceStorage    bool
	ChildResourcePoolName string
	ChildFolderName       string
	ClusterMoRef          vimtypes.ManagedObjectReference

	NetworkResults network.NetworkInterfaceResults
}

// TODO: Until we sort out what the Session becomes.
type vmUpdateArgs = session.VMUpdateArgs
type vmResizeArgs = session.VMResizeArgs

var (
	createCountLock       sync.Mutex
	concurrentCreateCount int

	// currentlyReconciling tracks the VMs currently being created in a
	// non-blocking goroutine.
	currentlyReconciling sync.Map

	// SkipVMImageCLProviderCheck skips the checks that a VM Image has a Content Library item provider
	// since a VirtualMachineImage created for a VM template won't have either. This has been broken for
	// a long time but was otherwise masked on how the tests used to be organized.
	SkipVMImageCLProviderCheck = false
)

func (vs *vSphereVMProvider) CreateOrUpdateVirtualMachine(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) error {

	_, err := vs.createOrUpdateVirtualMachine(ctx, vm, false)
	return err
}

func (vs *vSphereVMProvider) CreateOrUpdateVirtualMachineAsync(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) (<-chan error, error) {

	return vs.createOrUpdateVirtualMachine(ctx, vm, true)
}

func (vs *vSphereVMProvider) createOrUpdateVirtualMachine(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	async bool) (chan error, error) {

	logger := pkgutil.FromContextOrDefault(ctx)
	logger.V(4).Info("Entering createOrUpdateVirtualMachine")

	vmNamespacedName := vm.NamespacedName()

	if _, ok := currentlyReconciling.Load(vmNamespacedName); ok {
		// Do not process the VM again if it is already being reconciled in a
		// goroutine.
		return nil, providers.ErrReconcileInProgress
	}

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(
			ctx,
			vimtypes.ID{},
			vs.getOpID(ctx, vm, "createOrUpdateVM"),
		),
		Logger: logger,
		VM:     vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return nil, err
	}

	// Set the VC UUID annotation on the VM before attempting creation or
	// update. Among other things, the annotation facilitates differential
	// handling of restore and fail-over operations.
	if vm.Annotations == nil {
		vm.Annotations = make(map[string]string)
	}
	vCenterInstanceUUID := client.VimClient().ServiceContent.About.InstanceUuid
	vm.Annotations[vmopv1.ManagerID] = vCenterInstanceUUID

	// Check to see if the VM can be found on the underlying platform.
	foundVM, err := vs.getVM(vmCtx, client, false)
	if err != nil {
		vmCtx.Logger.Error(err, "failed to find vm")
		return nil, err
	}

	if foundVM != nil {
		// Mark that this is an update operation.
		ctxop.MarkUpdate(vmCtx)

		vmCtx.Logger.V(4).Info("found VM and updating")
		return nil, vs.updateVirtualMachine(vmCtx, foundVM, client)
	}

	// Mark that this is a create operation.
	ctxop.MarkCreate(vmCtx)

	// Do not allow more than N create threads/goroutines.
	//
	// - In blocking create mode, this ensures there are reconciler threads
	//   available to reconcile non-create operations.
	//
	// - In non-blocking create mode, this ensures the number of goroutines
	//   spawned to create VMs does not take up too much memory.
	allowed, decrementConcurrentCreatesFn := vs.vmCreateConcurrentAllowed(vmCtx)
	if !allowed {
		return nil, providers.ErrTooManyCreates
	}

	// cleanupFn tracks the function(s) that must be invoked upon leaving this
	// function during a blocking create or after an async create.
	cleanupFn := decrementConcurrentCreatesFn

	if !async {
		defer cleanupFn()

		vmCtx.Logger.V(4).Info("Doing a blocking create")

		createArgs, err := vs.getCreateArgs(vmCtx, client)
		if err != nil {
			return nil, err
		}

		if _, err := vs.createVirtualMachine(
			vmCtx,
			client,
			createArgs); err != nil {

			return nil, err
		}

		return nil, nil
	}

	if _, ok := currentlyReconciling.LoadOrStore(
		vmNamespacedName,
		struct{}{}); ok {

		// If the VM is already being created in a goroutine, then there is no
		// need to create it again.
		//
		// However, we need to make sure we decrement the number of concurrent
		// creates before returning.
		cleanupFn()
		return nil, providers.ErrReconcileInProgress
	}

	vmCtx.Logger.V(4).Info("Doing a non-blocking create")

	// Update the cleanup function to include indicating a concurrent create is
	// no longer occurring.
	cleanupFn = func() {
		currentlyReconciling.Delete(vmNamespacedName)
		decrementConcurrentCreatesFn()
	}

	createArgs, err := vs.getCreateArgs(vmCtx, client)
	if err != nil {
		// If getting the create args failed, the concurrent counter needs to be
		// decremented before returning.
		cleanupFn()
		return nil, err
	}

	// Create a copy of the context and replace its VM with a copy to
	// ensure modifications in the goroutine below are not impacted or
	// impact the operations above us in the call stack.
	copyOfCtx := vmCtx
	copyOfCtx.VM = vmCtx.VM.DeepCopy()

	// Start a goroutine to create the VM in the background.
	chanErr := make(chan error)
	go vs.createVirtualMachineAsync(
		copyOfCtx,
		client,
		createArgs,
		chanErr,
		cleanupFn)

	// Return with the error channel. The VM will be re-enqueued once the create
	// completes with success or failure.
	return chanErr, nil
}

func (vs *vSphereVMProvider) DeleteVirtualMachine(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) error {

	vmNamespacedName := vm.NamespacedName()

	if _, ok := currentlyReconciling.Load(vmNamespacedName); ok {
		// If the VM is already being reconciled in a goroutine then it cannot
		// be deleted yet.
		return providers.ErrReconcileInProgress
	}

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "deleteVM")),
		Logger:  pkgutil.FromContextOrDefault(ctx),
		VM:      vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return err
	}

	vcVM, err := vs.getVM(vmCtx, client, false)
	if err != nil {
		return err
	} else if vcVM == nil {
		// VM does not exist.
		return nil
	}

	return virtualmachine.DeleteVirtualMachine(vmCtx, vcVM)
}

func (vs *vSphereVMProvider) PublishVirtualMachine(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	vmPub *vmopv1.VirtualMachinePublishRequest,
	cl *imgregv1a1.ContentLibrary,
	actID string) (string, error) {

	logger := pkgutil.FromContextOrDefault(ctx).WithValues(
		"vmName", vm.NamespacedName(), "clName", fmt.Sprintf("%s/%s", cl.Namespace, cl.Name))
	ctx = logr.NewContext(ctx, logger)

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "publishVM")),
		Logger:  logger,
		VM:      vm,
	}

	client, err := vs.getVcClient(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get vCenter client: %w", err)
	}

	itemID, err := virtualmachine.CreateOVF(vmCtx, client.RestClient(), vmPub, cl, actID)
	if err != nil {
		return "", err
	}

	return itemID, nil
}

func (vs *vSphereVMProvider) GetVirtualMachineGuestHeartbeat(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) (vmopv1.GuestHeartbeatStatus, error) {

	logger := pkgutil.FromContextOrDefault(ctx).WithValues("vmName", vm.NamespacedName())
	ctx = logr.NewContext(ctx, logger)

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "heartbeat")),
		Logger:  logger,
		VM:      vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return "", err
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return "", err
	}

	status, err := virtualmachine.GetGuestHeartBeatStatus(vmCtx, vcVM)
	if err != nil {
		return "", err
	}

	return status, nil
}

func (vs *vSphereVMProvider) GetVirtualMachineProperties(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	propertyPaths []string) (map[string]any, error) {

	logger := pkgutil.FromContextOrDefault(ctx).WithValues("vmName", vm.NamespacedName())
	ctx = logr.NewContext(ctx, logger)

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "properties")),
		Logger:  logger,
		VM:      vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return nil, err
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return nil, err
	}

	propSet := []vimtypes.PropertySpec{{Type: "VirtualMachine"}}
	if len(propertyPaths) == 0 {
		propSet[0].All = vimtypes.NewBool(true)
	} else {
		propSet[0].PathSet = propertyPaths
	}

	rep, err := property.DefaultCollector(client.VimClient()).RetrieveProperties(
		ctx,
		vimtypes.RetrieveProperties{
			SpecSet: []vimtypes.PropertyFilterSpec{
				{
					ObjectSet: []vimtypes.ObjectSpec{
						{
							Obj: vcVM.Reference(),
						},
					},
					PropSet: propSet,
				},
			},
		})

	if err != nil {
		return nil, err
	}

	if len(rep.Returnval) == 0 {
		return nil, fmt.Errorf("no properties")
	}

	result := map[string]any{}
	for i := range rep.Returnval[0].PropSet {
		dp := rep.Returnval[0].PropSet[i]
		result[dp.Name] = dp.Val
	}

	return result, nil
}

func (vs *vSphereVMProvider) GetVirtualMachineWebMKSTicket(
	ctx context.Context,
	vm *vmopv1.VirtualMachine,
	pubKey string) (string, error) {

	logger := pkgutil.FromContextOrDefault(ctx).WithValues("vmName", vm.NamespacedName())
	ctx = logr.NewContext(ctx, logger)

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "webconsole")),
		Logger:  logger,
		VM:      vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return "", err
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return "", err
	}

	ticket, err := virtualmachine.GetWebConsoleTicket(vmCtx, vcVM, pubKey)
	if err != nil {
		return "", err
	}

	return ticket, nil
}

func (vs *vSphereVMProvider) GetVirtualMachineHardwareVersion(
	ctx context.Context,
	vm *vmopv1.VirtualMachine) (vimtypes.HardwareVersion, error) {

	logger := pkgutil.FromContextOrDefault(ctx).WithValues("vmName", vm.NamespacedName())
	ctx = logr.NewContext(ctx, logger)

	vmCtx := pkgctx.VirtualMachineContext{
		Context: context.WithValue(ctx, vimtypes.ID{}, vs.getOpID(ctx, vm, "hardware-version")),
		Logger:  logger,
		VM:      vm,
	}

	client, err := vs.getVcClient(vmCtx)
	if err != nil {
		return 0, err
	}

	vcVM, err := vs.getVM(vmCtx, client, true)
	if err != nil {
		return 0, err
	}

	var o mo.VirtualMachine
	err = vcVM.Properties(vmCtx, vcVM.Reference(), []string{"config.version"}, &o)
	if err != nil {
		return 0, err
	}

	return vimtypes.ParseHardwareVersion(o.Config.Version)
}

func (vs *vSphereVMProvider) vmCreatePathName(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	if len(vmCtx.VM.Spec.Cdrom) == 0 {
		return nil // only needed when deploying ISO library items
	}

	if createArgs.StorageProfileID == "" {
		return nil
	}
	if createArgs.ConfigSpec.Files == nil {
		createArgs.ConfigSpec.Files = &vimtypes.VirtualMachineFileInfo{}
	}
	if createArgs.ConfigSpec.Files.VmPathName != "" {
		return nil
	}

	vc := vcClient.VimClient()

	pc, err := pbm.NewClient(vmCtx, vc)
	if err != nil {
		return err
	}

	ds, err := pc.DatastoreMap(vmCtx, vc, createArgs.ClusterMoRef)
	if err != nil {
		return err
	}

	req := []pbmtypes.BasePbmPlacementRequirement{
		&pbmtypes.PbmPlacementCapabilityProfileRequirement{
			ProfileId: pbmtypes.PbmProfileId{UniqueId: createArgs.StorageProfileID},
		},
	}

	res, err := pc.CheckRequirements(vmCtx, ds.PlacementHub, nil, req)
	if err != nil {
		return err
	}

	hubs := res.CompatibleDatastores()
	if len(hubs) == 0 {
		return nil
	}

	hub := hubs[rand.Intn(len(hubs))] //nolint:gosec

	createArgs.ConfigSpec.Files.VmPathName = (&object.DatastorePath{
		Datastore: ds.Name[hub.HubId],
	}).String()

	vmCtx.Logger.Info("vmCreatePathName", "VmPathName", createArgs.ConfigSpec.Files.VmPathName)

	return nil
}

func (vs *vSphereVMProvider) vmCreatePathNameFromDatastoreRecommendation(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	if createArgs.ConfigSpec.Files == nil {
		createArgs.ConfigSpec.Files = &vimtypes.VirtualMachineFileInfo{}
	}
	if createArgs.ConfigSpec.Files.VmPathName != "" {
		return nil
	}
	if len(createArgs.Datastores) == 0 {
		return errors.New("no compatible datastores")
	}

	createArgs.ConfigSpec.Files.VmPathName = fmt.Sprintf(
		"[%s] %s/%s.vmx",
		createArgs.Datastores[0].Name,
		vmCtx.VM.UID,
		vmCtx.VM.Name)

	vmCtx.Logger.Info(
		"vmCreatePathName",
		"VmPathName", createArgs.ConfigSpec.Files.VmPathName)

	return nil
}

func (vs *vSphereVMProvider) getCreateArgs(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client) (*VMCreateArgs, error) {

	createArgs, err := vs.vmCreateGetArgs(vmCtx, vcClient)
	if err != nil {
		return nil, err
	}

	if err := vs.vmCreateDoPlacement(vmCtx, vcClient, createArgs); err != nil {
		return nil, err
	}

	if err := vs.vmCreateGetFolderAndRPMoIDs(vmCtx, vcClient, createArgs); err != nil {
		return nil, err
	}

	if pkgcfg.FromContext(vmCtx).Features.FastDeploy {
		if err := vs.vmCreateGetSourceFilePaths(vmCtx, vcClient, createArgs); err != nil {
			return nil, err
		}
		if err := vs.vmCreatePathNameFromDatastoreRecommendation(vmCtx, createArgs); err != nil {
			return nil, err
		}
	} else {
		if err := vs.vmCreatePathName(vmCtx, vcClient, createArgs); err != nil {
			return nil, err
		}
	}

	if err := vs.vmCreateIsReady(vmCtx, vcClient, createArgs); err != nil {
		return nil, err
	}

	return createArgs, nil
}

func (vs *vSphereVMProvider) createVirtualMachine(
	ctx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	args *VMCreateArgs) (*object.VirtualMachine, error) {

	moRef, err := vmlifecycle.CreateVirtualMachine(
		ctx,
		vs.k8sClient,
		vcClient.RestClient(),
		vcClient.VimClient(),
		vcClient.Finder(),
		&args.CreateArgs)

	if err != nil {
		ctx.Logger.Error(err, "CreateVirtualMachine failed")
		pkgcnd.MarkError(
			ctx.VM,
			vmopv1.VirtualMachineConditionCreated,
			"Error",
			err)

		if pkgcfg.FromContext(ctx).Features.FastDeploy {
			pkgcnd.MarkError(
				ctx.VM,
				vmopv1.VirtualMachineConditionPlacementReady,
				"Error",
				err)
		}

		return nil, err
	}

	ctx.VM.Status.UniqueID = moRef.Reference().Value
	pkgcnd.MarkTrue(ctx.VM, vmopv1.VirtualMachineConditionCreated)

	if pkgcfg.FromContext(ctx).Features.FastDeploy {
		if zoneName := args.ZoneName; zoneName != "" {
			if ctx.VM.Labels == nil {
				ctx.VM.Labels = map[string]string{}
			}
			ctx.VM.Labels[topology.KubernetesTopologyZoneLabelKey] = zoneName
		}
	}

	return object.NewVirtualMachine(vcClient.VimClient(), *moRef), ErrCreate
}

func (vs *vSphereVMProvider) createVirtualMachineAsync(
	ctx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	args *VMCreateArgs,
	chanErr chan error,
	cleanupFn func()) {

	defer func() {
		close(chanErr)
		cleanupFn()
	}()

	moRef, vimErr := vmlifecycle.CreateVirtualMachine(
		ctx,
		vs.k8sClient,
		vcClient.RestClient(),
		vcClient.VimClient(),
		vcClient.Finder(),
		&args.CreateArgs)

	if vimErr != nil {
		ctx.Logger.Error(vimErr, "CreateVirtualMachine failed")
		chanErr <- vimErr
	} else {
		chanErr <- ErrCreate
	}

	_, k8sErr := controllerutil.CreateOrPatch(
		ctx,
		vs.k8sClient,
		ctx.VM,
		func() error {

			if vimErr != nil {
				pkgcnd.MarkError(
					ctx.VM,
					vmopv1.VirtualMachineConditionCreated,
					"Error",
					vimErr)

				if pkgcfg.FromContext(ctx).Features.FastDeploy {
					pkgcnd.MarkError(
						ctx.VM,
						vmopv1.VirtualMachineConditionPlacementReady,
						"Error",
						vimErr)
				}

				return nil
			}

			if pkgcfg.FromContext(ctx).Features.FastDeploy {
				if zoneName := args.ZoneName; zoneName != "" {
					if ctx.VM.Labels == nil {
						ctx.VM.Labels = map[string]string{}
					}
					ctx.VM.Labels[topology.KubernetesTopologyZoneLabelKey] = zoneName
				}
			}

			ctx.VM.Status.UniqueID = moRef.Reference().Value
			pkgcnd.MarkTrue(ctx.VM, vmopv1.VirtualMachineConditionCreated)

			return nil
		},
	)

	if k8sErr != nil {
		ctx.Logger.Error(k8sErr, "Failed to patch VM status after create")
		chanErr <- k8sErr
	}
}

// VMUpdatePropertiesSelector is the set of VM properties fetched at the start
// of updateVirtualMachine.
var VMUpdatePropertiesSelector = []string{
	"config",
	"guest",
	"layoutEx",
	"resourcePool",
	"runtime",
	"snapshot",
	"summary",
}

func getReconcileErr(msg string, reconcileErr, err error) error {
	err = fmt.Errorf("failed to reconcile %s: %w", msg, err)
	if reconcileErr == nil {
		return err
	}
	return fmt.Errorf("%w, %w", err, reconcileErr)
}

func errOrReconcileErr(reconcileErr, err error) error {
	if reconcileErr != nil {
		return reconcileErr
	}
	return err
}

func (vs *vSphereVMProvider) updateVirtualMachine(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	vcClient *vcclient.Client) error {

	vmCtx.Logger.V(4).Info("Updating VirtualMachine")

	var reconcileErr error

	//
	// Fetch properties
	//
	if err := vcVM.Properties(
		vmCtx,
		vcVM.Reference(),
		VMUpdatePropertiesSelector,
		&vmCtx.MoVM); err != nil {

		return fmt.Errorf("failed to fetch vm properties: %w", err)
	}

	//
	// Reconcile a snapshot restore operation.
	//
	// Reverting a VM to a snapshot results in the VM's spec getting
	// overwritten from the snapshot. For now, we do not allow any
	// other changes to the VM's spec if a revert is being requested
	// to avoid having to merge the spec post revert.
	//
	if pkgcfg.FromContext(vmCtx).Features.VMSnapshots {
		if requeue, err := vs.reconcileSnapshotRevert(vmCtx, vcVM); err != nil {
			if pkgerr.IsNoRequeueError(err) {
				return errOrReconcileErr(reconcileErr, err)
			}
			reconcileErr = getReconcileErr("snapshot revert", reconcileErr, err)
		} else if requeue {
			// Snapshot revert was successful and requires requeue for next reconcile
			// to operate on the updated VM spec. Return any accumulated reconcile errors.
			vmCtx.Logger.V(4).Info("Snapshot revert completed successfully, requeuing reconcile")
			return reconcileErr
		}
	}

	//
	// Reconcile schema upgrade
	//
	if err := vs.reconcileSchemaUpgrade(vmCtx); err != nil {
		if pkgerr.IsNoRequeueError(err) {
			return errOrReconcileErr(reconcileErr, err)
		}
		reconcileErr = getReconcileErr("schema upgrade", reconcileErr, err)
	}

	//
	// Reconcile status
	//
	if err := vs.reconcileStatus(vmCtx, vcVM); err != nil {
		if pkgerr.IsNoRequeueError(err) {
			return errOrReconcileErr(reconcileErr, err)
		}
		reconcileErr = getReconcileErr("status", reconcileErr, err)
	}

	//
	// Reconcile backup state (VKS nodes excluded)
	//
	if err := vs.reconcileBackupState(vmCtx, vcVM); err != nil {
		if pkgerr.IsNoRequeueError(err) {
			return errOrReconcileErr(reconcileErr, err)
		}
		reconcileErr = getReconcileErr("backup state", reconcileErr, err)
	}

	//
	// Reconcile config
	//
	if err := vs.reconcileConfig(vmCtx, vcVM, vcClient); err != nil {
		if pkgerr.IsNoRequeueError(err) {
			return errOrReconcileErr(reconcileErr, err)
		}
		reconcileErr = getReconcileErr("config", reconcileErr, err)

		// Only return the error if the VM is *not* going from powered on to
		// off. This allows a VM to be powered off even if there is a reconfig
		// error.
		if !vmCtx.IsOnToOff() {
			return reconcileErr
		}
	}

	//
	// Reconcile power state
	//
	if err := vs.reconcilePowerState(vmCtx, vcVM); err != nil {
		if pkgerr.IsNoRequeueError(err) {
			return errOrReconcileErr(reconcileErr, err)
		}
		reconcileErr = getReconcileErr("power state", reconcileErr, err)
	}

	// Reconcile the current snapshot of this VM.
	if pkgcfg.FromContext(vmCtx).Features.VMSnapshots {
		if err := vs.reconcileSnapshot(vmCtx, vcVM); err != nil {
			if pkgerr.IsNoRequeueError(err) {
				return err
			}
			return fmt.Errorf("failed to reconcile the current snapshot: %w", err)
		}
	}

	return reconcileErr
}

func (vs *vSphereVMProvider) reconcileStatus(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) error {

	vmCtx.Logger.V(4).Info("Reconciling status")

	var networkDeviceKeysToSpecIdx map[int32]int
	if vmCtx.MoVM.Config != nil {
		networkDeviceKeysToSpecIdx = network.MapEthernetDevicesToSpecIdx(
			vmCtx, vs.k8sClient, vmCtx.MoVM)
	}

	return vmlifecycle.ReconcileStatus(
		vmCtx,
		vs.k8sClient,
		vcVM,
		vmlifecycle.ReconcileStatusData{
			NetworkDeviceKeysToSpecIdx: networkDeviceKeysToSpecIdx,
		})
}

func (vs *vSphereVMProvider) reconcileSchemaUpgrade(
	vmCtx pkgctx.VirtualMachineContext) error {

	return upgradevm.ReconcileSchemaUpgrade(
		vmCtx,
		vs.k8sClient,
		vmCtx.VM,
		vmCtx.MoVM)
}

func (vs *vSphereVMProvider) reconcileConfig(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	vcClient *vcclient.Client) error {

	vmCtx.Logger.V(4).Info("Reconciling config")

	if err := verifyConfigInfo(vmCtx); err != nil {
		return err
	}
	if err := verifyConnectionState(vmCtx); err != nil {
		return err
	}
	if err := verifyResourcePool(vmCtx); err != nil {
		return err
	}

	clusterMoRef, err := vcenter.GetResourcePoolOwnerMoRef(
		vmCtx,
		vcVM.Client(),
		vmCtx.MoVM.ResourcePool.Value)
	if err != nil {
		return err
	}

	ses := &session.Session{
		K8sClient:    vs.k8sClient,
		Client:       vcClient.Client,
		Finder:       vcClient.Finder(),
		ClusterMoRef: clusterMoRef,
	}

	getUpdateArgsFn := func() (*vmUpdateArgs, error) {
		return vs.vmUpdateGetArgs(vmCtx)
	}

	getResizeArgsFn := func() (*vmResizeArgs, error) {
		return vs.vmResizeGetArgs(vmCtx)
	}

	return ses.UpdateVirtualMachine(
		vmCtx,
		vcVM,
		getUpdateArgsFn,
		getResizeArgsFn)
}

func (vs *vSphereVMProvider) reconcileBackupState(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) error {

	vmCtx.Logger.V(4).Info("Reconciling backup state")

	if kubeutil.HasCAPILabels(vmCtx.VM.Labels) &&
		!metav1.HasAnnotation(
			vmCtx.VM.ObjectMeta,
			vmopv1.ForceEnableBackupAnnotation,
		) {

		return nil
	}

	vmCtx.Logger.V(4).Info("Backing up VM Service managed VM")

	diskUUIDToPVC, err := GetAttachedDiskUUIDToPVC(vmCtx, vs.k8sClient)
	if err != nil {
		vmCtx.Logger.Error(err, "failed to get disk uuid to PVC mapping for backup")
		return err
	}

	additionalResources, err := GetAdditionalResourcesForBackup(vmCtx, vs.k8sClient)
	if err != nil {
		vmCtx.Logger.Error(err, "failed to get additional resources for backup")
		return err
	}

	backupOpts := virtualmachine.BackupVirtualMachineOptions{
		VMCtx:               vmCtx,
		VcVM:                vcVM,
		DiskUUIDToPVC:       diskUUIDToPVC,
		AdditionalResources: additionalResources,
		BackupVersion:       fmt.Sprint(time.Now().UnixMilli()),
		ClassicDiskUUIDs:    GetAttachedClassicDiskUUIDs(vmCtx),
	}

	return virtualmachine.BackupVirtualMachine(backupOpts)
}

func (vs *vSphereVMProvider) reconcilePowerState(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) error {

	vmCtx.Logger.V(4).Info("Reconciling power state")

	if err := verifyConnectionState(vmCtx); err != nil {
		return err
	}
	if err := verifyResourcePool(vmCtx); err != nil {
		return err
	}

	// Check if the VM's power state should be delayed.
	// Currently, only power-on can be delayed by a parent group.
	if pkgcfg.FromContext(vmCtx).Features.VMGroups &&
		vmCtx.VM.Spec.PowerState == vmopv1.VirtualMachinePowerStateOn {
		if val := vmCtx.VM.Annotations[pkgconst.ApplyPowerStateTimeAnnotation]; val != "" {
			when, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				vmCtx.Logger.Error(err,
					"Failed to parse apply power state time from annotation",
					"annotationKey", pkgconst.ApplyPowerStateTimeAnnotation,
					"annotationValue", val)
				return err
			}
			if when.After(time.Now().UTC()) {
				// Apply power state time has not yet arrived.
				// Requeue the request with a delay of the remaining time.
				vmCtx.Logger.Info(
					"Skipping power state as the time has not yet arrived",
					"applyPowerStateTime", when)
				return pkgerr.RequeueError{
					After: time.Until(when),
				}
			}
			// Apply power state time has arrived. Remove the stale annotation
			// and continue the power state change.
			delete(vmCtx.VM.Annotations, pkgconst.ApplyPowerStateTimeAnnotation)
		}
	}

	if isVMPaused(vmCtx) {
		return ErrIsPaused
	}
	if vmCtx.VM.Status.TaskID != "" {
		return ErrHasTask
	}

	const (
		hard    = vmopv1.VirtualMachinePowerOpModeHard
		soft    = vmopv1.VirtualMachinePowerOpModeSoft
		trySoft = vmopv1.VirtualMachinePowerOpModeTrySoft
	)

	var (
		setPowerState     bool
		powerOpMode       vmopv1.VirtualMachinePowerOpMode
		currentPowerState vmopv1.VirtualMachinePowerState
		desiredPowerState = vmCtx.VM.Spec.PowerState
	)

	// Translate the VM's current power state into the VM Op power state
	// value.
	switch vmCtx.MoVM.Summary.Runtime.PowerState {
	case vimtypes.VirtualMachinePowerStatePoweredOn:
		currentPowerState = vmopv1.VirtualMachinePowerStateOn
	case vimtypes.VirtualMachinePowerStatePoweredOff:
		currentPowerState = vmopv1.VirtualMachinePowerStateOff
	case vimtypes.VirtualMachinePowerStateSuspended:
		currentPowerState = vmopv1.VirtualMachinePowerStateSuspended
	}

	switch desiredPowerState {

	case vmopv1.VirtualMachinePowerStateOn:

		if currentPowerState == vmopv1.VirtualMachinePowerStateOn {
			// Check to see if a possible restart is required.
			// Please note a VM may only be restarted if it is powered on.
			if vmCtx.VM.Spec.NextRestartTime == "" {
				return nil
			}

			// If non-empty, the value of spec.nextRestartTime is guaranteed
			// to be a valid RFC3339Nano timestamp due to the webhooks,
			// however, we still check for the error due to testing that may
			// not involve webhooks.
			nextRestartTime, err := time.Parse(
				time.RFC3339Nano, vmCtx.VM.Spec.NextRestartTime)
			if err != nil {
				return fmt.Errorf(
					"spec.nextRestartTime %q cannot be parsed with %q %w",
					vmCtx.VM.Spec.NextRestartTime, time.RFC3339Nano, err)
			}

			result, err := vmutil.RestartAndWait(
				logr.NewContext(vmCtx, vmCtx.Logger),
				vcVM.Client(),
				vmutil.ManagedObjectFromObject(vcVM),
				false,
				nextRestartTime,
				vmutil.ParsePowerOpMode(string(vmCtx.VM.Spec.RestartMode)))
			if err != nil {
				return err
			}
			if result.AnyChange() {
				lastRestartTime := metav1.NewTime(nextRestartTime)
				vmCtx.VM.Status.LastRestartTime = &lastRestartTime

				return ErrRestart
			}

			return nil
		}

		powerOpMode = hard

		for k, v := range vmCtx.VM.Annotations {
			if strings.HasPrefix(k, vmopv1.CheckAnnotationPowerOn+"/") {
				vmCtx.Logger.Info(
					"Skipping poweron due to annotation",
					"annotationKey", k, "annotationValue", v)
				return nil
			}
		}

		setPowerState = true

	case vmopv1.VirtualMachinePowerStateOff:
		powerOpMode = vmCtx.VM.Spec.PowerOffMode
		switch currentPowerState {
		case vmopv1.VirtualMachinePowerStateOn:
			setPowerState = true
		case vmopv1.VirtualMachinePowerStateSuspended:
			setPowerState = vmCtx.VM.Spec.PowerOffMode == hard ||
				vmCtx.VM.Spec.PowerOffMode == trySoft
		}

	case vmopv1.VirtualMachinePowerStateSuspended:
		powerOpMode = vmCtx.VM.Spec.SuspendMode
		setPowerState = currentPowerState == vmopv1.VirtualMachinePowerStateOn

	}

	if setPowerState {
		err := res.NewVMFromObject(vcVM).SetPowerState(
			vmCtx,
			currentPowerState,
			vmCtx.VM.Spec.PowerState,
			powerOpMode)

		if errors.Is(err, ErrSetPowerState) &&
			desiredPowerState == vmopv1.VirtualMachinePowerStateOn {

			if vmCtx.VM.Annotations == nil {
				vmCtx.VM.Annotations = map[string]string{}
			}
			vmCtx.VM.Annotations[vmopv1.FirstBootDoneAnnotation] = "true"
		}

		return err
	}

	return nil
}

func (vs *vSphereVMProvider) reconcileSnapshotRevert(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) (bool, error) {

	vmCtx.Logger.V(4).Info("Reconciling snapshot revert")

	// If no spec.CurrentSnapshot is specified, nothing to revert to.
	if vmCtx.VM.Spec.CurrentSnapshot == nil {
		vmCtx.Logger.V(5).Info("No current snapshot specified, skipping revert")
		return false, nil
	}

	// Check if the VM has a snapshot by the desired name.
	desiredSnapshotName := vmCtx.VM.Spec.CurrentSnapshot.Name
	snapObj, err := vs.findDesiredSnapshot(vmCtx, desiredSnapshotName)
	if err != nil {
		vmCtx.Logger.Error(err, "Error finding desired snapshot to revert to")
		return false, err
	}

	// Govmomi ensures that if FindSnapshot doesn't return an error, a
	// snapshot managed object is returned. Handle it anyway.
	if snapObj == nil {
		vmCtx.Logger.V(4).Info("Snapshot not found, treating as new snapshot request",
			"snapshotName", desiredSnapshotName)
		return false, nil
	}

	vmCtx.Logger.V(4).Info("Found desired snapshot for the VM", "snapshotRef", snapObj.Reference().Value)

	// Check if we need to revert (current snapshot != desired snapshot)
	if needsRevert := vs.needsSnapshotRevert(vmCtx, snapObj); !needsRevert {
		vmCtx.Logger.V(4).Info("VM already points to the desired snapshot",
			"snapshotName", desiredSnapshotName)
		return false, nil
	}

	vmCtx.Logger.Info("Starting snapshot revert operation",
		"snapshotName", desiredSnapshotName,
		"currentSnapshot", vmCtx.MoVM.Snapshot.CurrentSnapshot.Value,
		"desiredSnapshot", snapObj.Reference().Value)

	// Set the revert in progress annotation.
	vmCtx.Logger.V(4).Info("Setting revert in progress annotation")
	vs.setRevertInProgressAnnotation(vmCtx)

	// Perform the actual snapshot revert
	vmCtx.Logger.V(4).Info("Starting vSphere snapshot revert operation")
	if err := vs.performSnapshotRevert(vmCtx, vcVM, snapObj, desiredSnapshotName); err != nil {
		vmCtx.Logger.Error(err, "vSphere snapshot revert failed")
		return true, err
	}
	vmCtx.Logger.Info("vSphere snapshot revert completed successfully")

	// TODO: AKP: We will modify the snapshot workflow to always skip the automatic
	// registration.  Once we do that, we will have to remove that ExtraConfig key.

	// TODO: Move the parsing VM spec code above this section so we can bail out if
	// it's not a VM that we can restore.

	// Restore VM spec and metadata from the snapshot.
	vmCtx.Logger.Info("Starting VM spec and metadata restoration from snapshot")

	if err := vs.restoreVMSpecFromSnapshot(vmCtx, vcVM, snapObj); err != nil {
		vmCtx.Logger.Error(err, "Restoring VM spec and metadata from snapshot failed")
		return true, err
	}

	vmCtx.Logger.V(5).Info("VM spec restoration completed",
		"afterRestoreAnnotations", vmCtx.VM.Annotations,
		"afterRestoreLabels", vmCtx.VM.Labels,
		"afterRestoreSpecCurrentSnapshot", vmCtx.VM.Spec.CurrentSnapshot)

	vmCtx.Logger.Info("Successfully completed snapshot revert and restored VM spec, requeuing for next reconcile",
		"snapshotName", desiredSnapshotName)

	// Return immediately after successful revert and spec restoration
	// to trigger requeue. This ensures the next reconcile operates on
	// the updated VM spec, labels, and annotations. The VM status
	// will be updated in the subsequent reconcile loop.
	return true, nil
}

// findDesiredSnapshot finds the snapshot object by name, returns nil if not found.
func (vs *vSphereVMProvider) findDesiredSnapshot(
	vmCtx pkgctx.VirtualMachineContext,
	snapshotName string) (*vimtypes.ManagedObjectReference, error) {

	snapObj, err := virtualmachine.FindSnapshot(vmCtx, snapshotName)
	if err != nil {
		// Only skip (return nil) for expected cases where we should not fail:
		// 1. VM has no snapshots at all
		// 2. Snapshot by name doesn't exist (this would be a different error message)
		if errors.Is(err, virtualmachine.ErrNoSnapshots) {
			vmCtx.Logger.V(4).Info("VM has no snapshots, skipping",
				"snapshotName", snapshotName)
			return nil, nil
		}

		// Check if snapshot by name doesn't exist
		if errors.Is(err, virtualmachine.ErrSnapshotNotFound) {
			vmCtx.Logger.V(4).Info("Snapshot not found on the VM (snapshot doesn't exist)",
				"snapshotName", snapshotName)
			return nil, nil
		}

		// For all other errors (property collector error, multiple snapshots, etc.),
		// return the actual error and requeue the reconcile request.
		vmCtx.Logger.Error(err, "Error finding snapshot",
			"snapshotName", snapshotName)
		return nil, err
	}

	// snapObj is nil when VM has snapshots but the specific snapshot doesn't exist
	// This is the normal case when FindSnapshot succeeds but doesn't find the named snapshot.
	if snapObj == nil {
		vmCtx.Logger.V(4).Info("Snapshot not found on the VM (snapshot doesn't exist)",
			"snapshotName", snapshotName)
		return nil, nil
	}

	return snapObj, nil
}

// needsSnapshotRevert checks if the VM's current snapshot differs
// from the desired snapshot.
func (vs *vSphereVMProvider) needsSnapshotRevert(
	vmCtx pkgctx.VirtualMachineContext,
	desiredSnapObj *vimtypes.ManagedObjectReference) bool {

	if vmCtx.MoVM.Snapshot == nil || vmCtx.MoVM.Snapshot.CurrentSnapshot == nil {
		vmCtx.Logger.Info("VM has no current snapshot, revert needed")
		return true
	}

	curSnapshotMoref := vmCtx.MoVM.Snapshot.CurrentSnapshot
	if curSnapshotMoref.Value == desiredSnapObj.Reference().Value {
		// VM already points to the desired snapshot.
		return false
	}

	vmCtx.Logger.V(4).Info("VM snapshot mismatch detected",
		"currentSnapshot", curSnapshotMoref.Value,
		"desiredSnapshot", desiredSnapObj.Reference().Value)

	return true
}

// setRevertInProgressAnnotation sets the annotation indicating a revert is in progress.
func (vs *vSphereVMProvider) setRevertInProgressAnnotation(vmCtx pkgctx.VirtualMachineContext) {
	if vmCtx.VM.Annotations == nil {
		vmCtx.VM.Annotations = make(map[string]string)
	}
	vmCtx.VM.Annotations[pkgconst.VirtualMachineSnapshotRevertInProgressAnnotationKey] = ""
}

// performSnapshotRevert executes the actual snapshot revert operation.
func (vs *vSphereVMProvider) performSnapshotRevert(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	snapObj *vimtypes.ManagedObjectReference,
	snapshotName string) error {

	// TODO: AKP: Add support for suppresspoweron option.
	task, err := vcVM.RevertToSnapshot(vmCtx, snapObj.Value, false)
	if err != nil {
		vmCtx.Logger.Error(err, "revert to snapshot failed", "snapshotName", snapshotName)
		return err
	}

	// Wait for the revert task to complete
	taskInfo, err := task.WaitForResult(vmCtx)
	if err != nil {
		vmCtx.Logger.Error(err, "snapshot revert task failed",
			"snapshotName", snapshotName, "taskInfo", taskInfo)
		return err
	}

	vmCtx.Logger.Info("Successfully reverted VM to snapshot", "snapshotName", snapshotName)
	return nil
}

// restoreVMSpecFromSnapshot extracts and applies the VM spec from the snapshot.
func (vs *vSphereVMProvider) restoreVMSpecFromSnapshot(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	snapObj *vimtypes.ManagedObjectReference) error {

	vmYAML, err := vs.getVMYamlFromSnapshot(vmCtx, vcVM, snapObj)
	if err != nil {
		vmCtx.Logger.Error(err, "failed to parse VM spec from snapshot")
		return err
	}

	// Handle cases where the snapshot doesn't contain any VM spec and metadata.
	if vmYAML == "" {
		return fmt.Errorf("no VM YAML in snapshot config")
	}

	var vm vmopv1.VirtualMachine
	if err := yaml.Unmarshal([]byte(vmYAML), &vm); err != nil {
		vmCtx.Logger.Error(err, "failed to unmarshal the payload stored in the snapshot into a VM type")
		return err
	}

	// Swap the spec, annotations and labels. We don't need to worry
	// about pruning fields here since the backup process already
	// handles that.
	vmCtx.VM.Spec = *(&vm.Spec).DeepCopy()
	vmCtx.VM.Labels = maps.Clone(vm.Labels)
	vmCtx.VM.Annotations = maps.Clone(vm.Annotations)
	// Empty out the status.
	vmCtx.VM.Status = vmopv1.VirtualMachineStatus{}

	// Note that since we swapped out the annotations from the
	// snapshot, it will implicitly remove the "restore-in-progress"
	// annotation. But go ahead and clean it up again in case the
	// snapshot somehow contained this annotation.
	revertProgressKey := pkgconst.VirtualMachineSnapshotRevertInProgressAnnotationKey
	delete(vmCtx.VM.Annotations, revertProgressKey)

	vmCtx.Logger.Info("VM custom resource has been restored from snapshot",
		"restoredAnnotations", vmCtx.VM.Annotations,
		"restoredLabels", vmCtx.VM.Labels)

	return nil
}

// getVMYamlFromSnapshot extracts the VM YAML from snapshot's ExtraConfig.
func (vs *vSphereVMProvider) getVMYamlFromSnapshot(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine,
	snap *vimtypes.ManagedObjectReference) (string, error) {

	var moSnap mo.VirtualMachineSnapshot

	// Fetch the config stored with the snapshot.
	if err := vcVM.Properties(vmCtx, *snap, []string{"config"}, &moSnap); err != nil {
		vmCtx.Logger.Error(err, "error fetching the snapshot config")
		return "", err
	}

	vmCtx.Logger.V(5).Info("Successfully fetched snapshot properties")

	ecList := object.OptionValueList(moSnap.Config.ExtraConfig)
	raw, exists := ecList.GetString(backupapi.VMResourceYAMLExtraConfigKey)
	if !exists {
		vmCtx.Logger.V(4).Info("VM does not have the necessary ExtraConfig in the Snapshot",
			"expectedKey", backupapi.VMResourceYAMLExtraConfigKey)
		// TODO: VMSVC-2709
		// Note that an imported VM could have snapshots that do not
		// have this backup key set. In this case, we need to
		// approximately generate the spec from the ConfigInfo.
		// We also set the ExtraConfig so we do not have to do this
		// when this VM is reverted to the same snapshot in future
		// again.
		return "", nil
	}

	vmCtx.Logger.V(5).Info("Found backup data in snapshot config",
		"key", backupapi.VMResourceYAMLExtraConfigKey,
		"dataLength", len(raw))

	// Uncompress and decode the data
	data, err := pkgutil.TryToDecodeBase64Gzip([]byte(raw))
	if err != nil {
		vmCtx.Logger.Error(err, "error in decoding the payload from snapshot")
		return "", err
	}

	return data, nil
}

func (vs *vSphereVMProvider) reconcileSnapshot(
	vmCtx pkgctx.VirtualMachineContext,
	vcVM *object.VirtualMachine) error {
	// Skip creating a new snapshot if the VM doesn't specify a desired snapshot.
	if vmCtx.VM.Spec.CurrentSnapshot == nil {
		return nil
	}

	// Fetch the VirtualMachineSnapshot object.
	vmSnapshot, err := getVirtualMachineSnapShotObject(vmCtx, vs.k8sClient)
	if err != nil {
		return err
	}

	// If it's being deleted, skip snapshot creation.
	if !vmSnapshot.DeletionTimestamp.IsZero() {
		vmCtx.Logger.Info("Skipping snapshot creation because the VirtualMachineSnapshot is being deleted")
		return nil
	}

	snapArgs := virtualmachine.SnapshotArgs{
		VMCtx:      vmCtx,
		VcVM:       vcVM,
		VMSnapshot: vmSnapshot,
	}

	vmCtx.Logger.Info("Creating a new snapshot of the virtual machine")
	snapMoRef, err := virtualmachine.SnapshotVirtualMachine(snapArgs)
	if err != nil {
		vmCtx.Logger.Error(err, "failed to snapshot virtual machine")
		return err
	}

	if err = PatchSnapshotStatus(vmCtx, vs.k8sClient,
		&vmSnapshot, snapMoRef); err != nil {
		return err
	}

	/* TODO:
	 * - Signal CSI to sync their volume snapshot quota
	 * - Update our storage quota usage
	 * - Update the VM storage quota usage
	 * - Update the VM status to reflect the new current snapshot
	 */

	return nil
}

func verifyConfigInfo(vmCtx pkgctx.VirtualMachineContext) error {
	if vmCtx.MoVM.Config == nil {
		return pkgerr.NoRequeueError{
			Message: "nil config info",
		}
	}
	return nil
}

func verifyConnectionState(vmCtx pkgctx.VirtualMachineContext) error {
	// Only reconcile connected VMs or if the connection state is empty.
	if cs := vmCtx.MoVM.Summary.Runtime.ConnectionState; cs != "" && cs !=
		vimtypes.VirtualMachineConnectionStateConnected {

		// Return a NoRequeueError so the VM is not requeued for
		// reconciliation.
		//
		// The watcher service ensures that VMs will be reconciled
		// immediately upon their summary.runtime.connectionState value
		// changing.
		//
		// TODO(akutz) Determine if we should surface some type of condition
		//             that indicates this state.
		return pkgerr.NoRequeueError{
			Message: fmt.Sprintf("unsupported connection state: %s", cs),
		}
	}

	return nil
}

func verifyResourcePool(vmCtx pkgctx.VirtualMachineContext) error {
	if vmCtx.MoVM.ResourcePool == nil {
		// Same error as govmomi VirtualMachine::ResourcePool().
		return pkgerr.NoRequeueError{
			Message: "VM does not belong to a resource pool",
		}
	}
	return nil
}

// vmCreateDoPlacement determines placement of the VM prior to creating the VM
// on VC. If VM has a group name specified, placement is determined by group.
func (vs *vSphereVMProvider) vmCreateDoPlacement(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) (retErr error) {

	defer func() {
		if retErr != nil {
			pkgcnd.MarkError(
				vmCtx.VM,
				vmopv1.VirtualMachineConditionPlacementReady,
				"NotReady",
				retErr)
		} else {
			pkgcnd.MarkTrue(
				vmCtx.VM,
				vmopv1.VirtualMachineConditionPlacementReady)
		}
	}()

	if pkgcfg.FromContext(vmCtx).Features.VMGroups &&
		vmCtx.VM.Spec.GroupName != "" {
		vmCtx.Logger.Info(
			"Getting VM placement result from its group",
			"groupName", vmCtx.VM.Spec.GroupName,
		)

		return vs.vmCreateDoPlacementByGroup(vmCtx, vcClient, createArgs)
	}

	placementConfigSpec, err := virtualmachine.CreateConfigSpecForPlacement(
		vmCtx,
		createArgs.ConfigSpec,
		createArgs.Storage.StorageClassToPolicyID)
	if err != nil {
		return err
	}

	pvcZones, err := kubeutil.GetPVCZoneConstraints(
		createArgs.Storage.StorageClasses,
		createArgs.Storage.PVCs)
	if err != nil {
		return err
	}

	constraints := placement.Constraints{
		ChildRPName: createArgs.ChildResourcePoolName,
		Zones:       pvcZones,
	}

	result, err := placement.Placement(
		vmCtx,
		vs.k8sClient,
		vcClient.VimClient(),
		vcClient.Finder(),
		placementConfigSpec,
		constraints)
	if err != nil {
		return err
	}

	return processPlacementResult(vmCtx, vcClient, createArgs, *result)
}

// vmCreateDoPlacementByGroup places the VM from the group's placement result.
func (vs *vSphereVMProvider) vmCreateDoPlacementByGroup(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	// Should never happen when this function is called, check just in case.
	if vmCtx.VM.Spec.GroupName == "" {
		return fmt.Errorf("VM.Spec.GroupName is empty")
	}

	var vmg vmopv1.VirtualMachineGroup
	if err := vs.k8sClient.Get(
		vmCtx,
		ctrlclient.ObjectKey{
			Namespace: vmCtx.VM.Namespace,
			Name:      vmCtx.VM.Spec.GroupName,
		},
		&vmg,
	); err != nil {
		return fmt.Errorf("failed to get VM Group object: %w", err)
	}

	var memberStatus vmopv1.VirtualMachineGroupMemberStatus
	for _, m := range vmg.Status.Members {
		if m.Kind == vmCtx.VM.Kind && m.Name == vmCtx.VM.Name {
			memberStatus = m
			break
		}
	}

	if !pkgcnd.IsTrue(
		&memberStatus,
		vmopv1.VirtualMachineGroupMemberConditionGroupLinked,
	) {
		return fmt.Errorf("VM is not linked to its group")
	}

	if !pkgcnd.IsTrue(
		&memberStatus,
		vmopv1.VirtualMachineGroupMemberConditionPlacementReady,
	) {
		return fmt.Errorf("VM Group placement is not ready")
	}

	placementStatus := memberStatus.Placement
	if placementStatus == nil {
		return fmt.Errorf("VM Group placement is empty")
	}

	vmCtx.Logger.V(6).Info(
		"VM Group placement is ready",
		"placement", placementStatus,
	)

	// Create a placement result from the group's placement status.
	var result placement.Result

	if placementStatus.Zone != "" {
		result.ZonePlacement = true
		result.ZoneName = placementStatus.Zone
	}

	if placementStatus.Node != "" {
		result.HostMoRef = &vimtypes.ManagedObjectReference{
			Type:  string(vimtypes.ManagedObjectTypesHostSystem),
			Value: placementStatus.Node,
		}
	}

	if placementStatus.Pool != "" {
		result.PoolMoRef = vimtypes.ManagedObjectReference{
			Type:  string(vimtypes.ManagedObjectTypesResourcePool),
			Value: placementStatus.Pool,
		}
	}

	result.Datastores = make([]placement.DatastoreResult, len(placementStatus.Datastores))
	for i, ds := range placementStatus.Datastores {
		if val := ds.DiskKey; val != nil {
			result.Datastores[i].DiskKey = *val
			result.Datastores[i].ForDisk = true
		}

		result.Datastores[i].MoRef = vimtypes.ManagedObjectReference{
			Type:  string(vimtypes.ManagedObjectTypesDatastore),
			Value: ds.ID,
		}

		result.Datastores[i].Name = ds.Name
		result.Datastores[i].URL = ds.URL
		result.Datastores[i].DiskFormats = ds.SupportedDiskFormats
	}

	// InstanceStoragePlacement flag is needed to update the VM's annotations
	// with the selected host for VMs that have instance storage backed volumes.
	// They're likely not supported for group placement. Leave it here for now
	// to keep consistent with the single VM placement workflow.
	if pkgcfg.FromContext(vmCtx).Features.InstanceStorage {
		if vmopv1util.IsInstanceStoragePresent(vmCtx.VM) {
			result.InstanceStoragePlacement = true
		}
	}

	return processPlacementResult(vmCtx, vcClient, createArgs, result)
}

// processPlacementResult updates the createArgs and VM annotations/labels with
// the given placement result.
func processPlacementResult(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs,
	result placement.Result) error {

	if result.PoolMoRef.Value != "" {
		createArgs.ResourcePoolMoID = result.PoolMoRef.Value
	}

	if result.HostMoRef != nil {
		createArgs.HostMoID = result.HostMoRef.Value
	}

	if pkgcfg.FromContext(vmCtx).Features.FastDeploy {
		createArgs.DatacenterMoID = vcClient.Datacenter().Reference().Value
		createArgs.Datastores = make([]vmlifecycle.DatastoreRef, len(result.Datastores))
		for i := range result.Datastores {
			createArgs.Datastores[i].DiskKey = result.Datastores[i].DiskKey
			createArgs.Datastores[i].ForDisk = result.Datastores[i].ForDisk
			createArgs.Datastores[i].MoRef = result.Datastores[i].MoRef
			createArgs.Datastores[i].Name = result.Datastores[i].Name
			createArgs.Datastores[i].URL = result.Datastores[i].URL
			createArgs.Datastores[i].DiskFormats = result.Datastores[i].DiskFormats
		}
	}

	if result.InstanceStoragePlacement {
		hostMoID := createArgs.HostMoID

		if hostMoID == "" {
			return fmt.Errorf("placement result missing host required for instance storage")
		}

		hostFQDN, err := vcenter.GetESXHostFQDN(vmCtx, vcClient.VimClient(), hostMoID)
		if err != nil {
			return err
		}

		if vmCtx.VM.Annotations == nil {
			vmCtx.VM.Annotations = map[string]string{}
		}
		vmCtx.VM.Annotations[constants.InstanceStorageSelectedNodeMOIDAnnotationKey] = hostMoID
		vmCtx.VM.Annotations[constants.InstanceStorageSelectedNodeAnnotationKey] = hostFQDN
	}

	if result.ZonePlacement {
		if pkgcfg.FromContext(vmCtx).Features.FastDeploy {
			createArgs.ZoneName = result.ZoneName
		} else {
			if vmCtx.VM.Labels == nil {
				vmCtx.VM.Labels = map[string]string{}
			}
			// Note if the VM create fails for some reason, but this label gets updated on the k8s VM,
			// then this is the pre-assigned zone on later create attempts.
			vmCtx.VM.Labels[topology.KubernetesTopologyZoneLabelKey] = result.ZoneName
		}
	}

	return nil
}

// vmCreateGetFolderAndRPMoIDs gets the MoIDs of the Folder and Resource Pool the VM will be created under.
func (vs *vSphereVMProvider) vmCreateGetFolderAndRPMoIDs(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	if createArgs.ResourcePoolMoID == "" {
		// We did not do placement so find this namespace/zone ResourcePool and Folder.

		nsFolderMoID, rpMoID, err := topology.GetNamespaceFolderAndRPMoID(vmCtx, vs.k8sClient,
			vmCtx.VM.Labels[topology.KubernetesTopologyZoneLabelKey], vmCtx.VM.Namespace)
		if err != nil {
			return err
		}

		// If this VM has a ResourcePolicy ResourcePool, lookup the child ResourcePool under the
		// namespace/zone's root ResourcePool. This will be the VM's ResourcePool.
		if createArgs.ChildResourcePoolName != "" {
			parentRP := object.NewResourcePool(vcClient.VimClient(),
				vimtypes.ManagedObjectReference{Type: "ResourcePool", Value: rpMoID})

			childRP, err := vcenter.GetChildResourcePool(vmCtx, parentRP, createArgs.ChildResourcePoolName)
			if err != nil {
				return err
			}

			rpMoID = childRP.Reference().Value
		}

		createArgs.ResourcePoolMoID = rpMoID
		createArgs.FolderMoID = nsFolderMoID

	} else {
		// Placement already selected the ResourcePool/Cluster, so we just need this namespace's Folder.
		nsFolderMoID, err := topology.GetNamespaceFolderMoID(vmCtx, vs.k8sClient, vmCtx.VM.Namespace)
		if err != nil {
			return err
		}

		createArgs.FolderMoID = nsFolderMoID
	}

	// If this VM has a ResourcePolicy Folder, lookup the child Folder under the namespace's Folder.
	// This will be the VM's parent Folder in the VC inventory.
	if createArgs.ChildFolderName != "" {
		parentFolder := object.NewFolder(vcClient.VimClient(),
			vimtypes.ManagedObjectReference{Type: "Folder", Value: createArgs.FolderMoID})

		childFolder, err := vcenter.GetChildFolder(vmCtx, parentFolder, createArgs.ChildFolderName)
		if err != nil {
			return err
		}

		createArgs.FolderMoID = childFolder.Reference().Value
	}

	// Now that we know the ResourcePool, use that to look up the CCR.
	clusterMoRef, err := vcenter.GetResourcePoolOwnerMoRef(vmCtx, vcClient.VimClient(), createArgs.ResourcePoolMoID)
	if err != nil {
		return err
	}
	createArgs.ClusterMoRef = clusterMoRef

	return nil
}

// vmCreateGetSourceFilePaths gets paths to the source file(s) used to create
// the VM.
func (vs *vSphereVMProvider) vmCreateGetSourceFilePaths(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	if len(createArgs.Datastores) == 0 {
		return errors.New("no compatible datastores")
	}

	var (
		datacenterID = vs.vcClient.Datacenter().Reference().Value
		datastoreID  = createArgs.Datastores[0].MoRef.Value
		itemID       = createArgs.ImageStatus.ProviderItemID
		itemVersion  = createArgs.ImageStatus.ProviderContentVersion
	)

	// Create/patch/get the VirtualMachineImageCache resource.
	obj := vmopv1.VirtualMachineImageCache{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pkgcfg.FromContext(vmCtx).PodNamespace,
			Name:      pkgutil.VMIName(itemID),
		},
	}
	if _, err := controllerutil.CreateOrPatch(
		vmCtx,
		vs.k8sClient,
		&obj,
		func() error {
			obj.Spec.ProviderID = itemID
			obj.Spec.ProviderVersion = itemVersion
			obj.AddLocation(
				datacenterID,
				datastoreID,
				createArgs.StorageProfileID)
			return nil
		}); err != nil {
		return fmt.Errorf(
			"failed to createOrPatch image cache resource: %w", err)
	}

	// Check if the files are cached.
	for i := range obj.Status.Locations {
		l := obj.Status.Locations[i]
		if l.DatacenterID == datacenterID && l.DatastoreID == datastoreID {
			if c := pkgcnd.Get(l, vmopv1.ReadyConditionType); c != nil {
				switch c.Status {
				case metav1.ConditionTrue:

					// Verify the files are still cached. If the files are
					// found to no longer exist, a reconcile request is enqueued
					// for the VMI cache object.
					if vs.vmCreateGetSourceFilePathsVerify(
						vmCtx,
						vcClient,
						obj,
						l.Files) {

						// The location has the cached files.
						vmCtx.Logger.Info("got source files", "files", l.Files)

						// Update the createArgs.DiskPaths with the paths from
						// the cached files slice.
						for i := range l.Files {
							id := l.Files[i].ID
							switch {
							case strings.EqualFold(".vmdk", path.Ext(id)):
								createArgs.DiskPaths = append(createArgs.DiskPaths, id)
							default:
								createArgs.FilePaths = append(createArgs.FilePaths, id)
							}
						}

						return nil
					}

				case metav1.ConditionFalse:
					// The files could not be cached at that location.
					return fmt.Errorf("failed to cache files: %s", c.Message)
				}
			}
		}
	}

	// The cached files are not yet ready, so return the following error.
	return pkgerr.VMICacheNotReadyError{
		Message:      "cached files not ready",
		Name:         obj.Name,
		DatacenterID: datacenterID,
		DatastoreID:  datastoreID,
	}
}

// vmCreateGetSourceFilePathsVerify verifies the provided file(s) are still
// available. If not, a reconcile request is enqueued for the VMI cache object.
func (vs *vSphereVMProvider) vmCreateGetSourceFilePathsVerify(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	obj vmopv1.VirtualMachineImageCache,
	srcFiles []vmopv1.VirtualMachineImageCacheFileStatus) bool {

	for i := range srcFiles {
		s := srcFiles[i]
		if err := pkgutil.DatastoreFileExists(
			vmCtx,
			vcClient.VimClient(),
			s.ID,
			vcClient.Datacenter()); err != nil {

			vmCtx.Logger.Error(err, "file is invalid", "filePath", s.ID)

			chanSource := cource.FromContextWithBuffer(
				vmCtx, "VirtualMachineImageCache", 100)
			chanSource <- event.GenericEvent{
				Object: &vmopv1.VirtualMachineImageCache{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: obj.Namespace,
						Name:      obj.Name,
					},
				},
			}

			return false
		}
	}

	return true
}

func (vs *vSphereVMProvider) vmCreateIsReady(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	if policy := createArgs.ResourcePolicy; policy != nil {
		// TODO: May want to do this as to filter the placement candidates.
		clusterModuleProvider := clustermodules.NewProvider(vcClient.RestClient())
		exists, err := vs.doClusterModulesExist(vmCtx, clusterModuleProvider, createArgs.ClusterMoRef, policy)
		if err != nil {
			return err
		} else if !exists {
			return fmt.Errorf("VirtualMachineSetResourcePolicy cluster module is not ready")
		}
	}

	if createArgs.HasInstanceStorage {
		if _, ok := vmCtx.VM.Annotations[constants.InstanceStoragePVCsBoundAnnotationKey]; !ok {
			return fmt.Errorf("instance storage PVCs are not bound yet")
		}
	}

	return nil
}

func (vs *vSphereVMProvider) vmCreateConcurrentAllowed(vmCtx pkgctx.VirtualMachineContext) (bool, func()) {
	maxDeployThreads := pkgcfg.FromContext(vmCtx).GetMaxDeployThreadsOnProvider()

	createCountLock.Lock()
	if concurrentCreateCount >= maxDeployThreads {
		createCountLock.Unlock()
		vmCtx.Logger.Info("Too many create VirtualMachine already occurring. Re-queueing request")
		return false, nil
	}

	concurrentCreateCount++
	createCountLock.Unlock()

	decrementFn := func() {
		createCountLock.Lock()
		concurrentCreateCount--
		createCountLock.Unlock()
	}

	return true, decrementFn
}

func (vs *vSphereVMProvider) vmCreateGetArgs(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client) (*VMCreateArgs, error) {

	createArgs, err := vs.vmCreateGetPrereqs(vmCtx, vcClient)
	if err != nil {
		return nil, err
	}

	err = vs.vmCreateDoNetworking(vmCtx, vcClient, createArgs)
	if err != nil {
		return nil, err
	}

	err = vs.vmCreateGenConfigSpec(vmCtx, createArgs)
	if err != nil {
		return nil, err
	}

	return createArgs, nil
}

// vmCreateGetPrereqs returns the VMCreateArgs populated with the k8s objects required to
// create the VM on VC.
func (vs *vSphereVMProvider) vmCreateGetPrereqs(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client) (*VMCreateArgs, error) {

	createArgs := &VMCreateArgs{}
	var prereqErrs []error

	if err := vs.vmCreateGetVirtualMachineClass(vmCtx, createArgs); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	if err := vs.vmCreateGetVirtualMachineImage(vmCtx, createArgs); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	if err := vs.vmCreateGetSetResourcePolicy(vmCtx, createArgs); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	if err := vs.vmCreateGetBootstrap(vmCtx, createArgs); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	if err := vs.vmCreateGetStoragePrereqs(vmCtx, vcClient, createArgs); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	if err := vmlifecycle.UpdateGroupLinkedCondition(vmCtx, vs.k8sClient); err != nil {
		prereqErrs = append(prereqErrs, err)
	}

	// This is about the point where historically we'd declare the prereqs ready or not. There
	// is still a lot of work to do - and things to fail - before the actual create, but there
	// is no point in continuing if the above checks aren't met since we are missing data
	// required to create the VM.
	if len(prereqErrs) > 0 {
		return nil, apierrorsutil.NewAggregate(prereqErrs)
	}

	if !vmopv1util.IsClasslessVM(*vmCtx.VM) {
		// Only set VM Class field for non-synthesized classes.
		if f := pkgcfg.FromContext(vmCtx).Features; f.VMResize || f.VMResizeCPUMemory {
			vmopv1util.MustSetLastResizedAnnotation(vmCtx.VM, createArgs.VMClass)
		}
		vmCtx.VM.Status.Class = &common.LocalObjectRef{
			APIVersion: vmopv1.GroupVersion.String(),
			Kind:       createArgs.VMClass.Kind,
			Name:       createArgs.VMClass.Name,
		}
	}

	return createArgs, nil
}

func (vs *vSphereVMProvider) vmCreateGetVirtualMachineClass(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	vmClass, err := GetVirtualMachineClass(vmCtx, vs.k8sClient)
	if err != nil {
		return err
	}

	createArgs.VMClass = vmClass

	return nil
}

func (vs *vSphereVMProvider) vmCreateGetVirtualMachineImage(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	imageObj, imageSpec, imageStatus, err := GetVirtualMachineImageSpecAndStatus(vmCtx, vs.k8sClient)
	if err != nil {
		return err
	}

	createArgs.ImageObj = imageObj
	createArgs.ImageSpec = imageSpec
	createArgs.ImageStatus = imageStatus

	var providerRef common.LocalObjectRef
	if imageSpec.ProviderRef != nil {
		providerRef = *imageSpec.ProviderRef
	}

	// This is clunky, but we need to know how to use the image to create the VM. Our only supported
	// method is via the ContentLibrary, so check if this image was derived from a CL item.
	switch providerRef.Kind {
	case "ClusterContentLibraryItem", "ContentLibraryItem":
		createArgs.UseContentLibrary = true
		createArgs.ProviderItemID = imageStatus.ProviderItemID
	default:
		if !SkipVMImageCLProviderCheck {
			err := fmt.Errorf("unsupported image provider kind: %s", providerRef.Kind)
			pkgcnd.MarkError(vmCtx.VM, vmopv1.VirtualMachineConditionImageReady, "NotSupported", err)
			return err
		}
		// Testing only: we'll clone the source VM found in the Inventory.
		createArgs.UseContentLibrary = false
		createArgs.ProviderItemID = vmCtx.VM.Spec.Image.Name
	}

	return nil
}

func (vs *vSphereVMProvider) vmCreateGetSetResourcePolicy(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	resourcePolicy, err := GetVMSetResourcePolicy(vmCtx, vs.k8sClient)
	if err != nil {
		return err
	}

	// The SetResourcePolicy is optional (TKG VMs will always have it).
	if resourcePolicy != nil {
		createArgs.ResourcePolicy = resourcePolicy
		createArgs.ChildFolderName = resourcePolicy.Spec.Folder
		createArgs.ChildResourcePoolName = resourcePolicy.Spec.ResourcePool.Name
	}

	return nil
}

func (vs *vSphereVMProvider) vmCreateGetBootstrap(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	bsData, err := GetVirtualMachineBootstrap(vmCtx, vs.k8sClient)
	if err != nil {
		return err
	}

	createArgs.BootstrapData = bsData

	return nil
}

func (vs *vSphereVMProvider) vmCreateGetStoragePrereqs(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	if pkgcfg.FromContext(vmCtx).Features.InstanceStorage {
		// To determine all the storage profiles, we need the class because of
		// the possibility of InstanceStorage volumes. If we weren't able to get
		// the class earlier, still check & set the storage condition because
		// instance storage usage is rare, it is helpful to report as many
		// prereqs as possible, and we'll reevaluate this once the class is
		// available.
		//
		// Add the class's instance storage disks - if any - to the VM.Spec.
		// Once the instance storage disks are added to the VM, they are set in
		// stone even if the class itself or the VM's assigned class changes.
		createArgs.HasInstanceStorage = AddInstanceStorageVolumes(
			vmCtx,
			createArgs.VMClass.Spec.Hardware.InstanceStorage)
	}

	vmStorageClass := vmCtx.VM.Spec.StorageClass
	if vmStorageClass == "" {
		cfg := vcClient.Config()

		// This will be true in WCP.
		if cfg.StorageClassRequired {
			err := fmt.Errorf("StorageClass is required but not specified")
			pkgcnd.MarkError(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady, "StorageClassRequired", err)
			return err
		}

		// Testing only for standalone gce2e.
		if cfg.Datastore == "" {
			err := fmt.Errorf("no Datastore provided in configuration")
			pkgcnd.MarkError(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady, "DatastoreNotFound", err)
			return err
		}

		datastore, err := vcClient.Finder().Datastore(vmCtx, cfg.Datastore)
		if err != nil {
			pkgcnd.MarkError(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady, "DatastoreNotFound", err)
			return fmt.Errorf("failed to find Datastore %s: %w", cfg.Datastore, err)
		}

		createArgs.DatastoreMoID = datastore.Reference().Value
	}

	vmStorage, err := storage.GetVMStorageData(vmCtx, vs.k8sClient)
	if err != nil {
		reason, msg := errToConditionReasonAndMessage(err)
		pkgcnd.MarkFalse(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady, reason, "%s", msg)
		return err
	}

	storageProfileID := vmStorage.StorageClassToPolicyID[vmStorageClass]
	isEnc, _, err := kubeutil.IsEncryptedStorageClass(
		vmCtx, vs.k8sClient, vmCtx.VM.Spec.StorageClass)
	if err != nil {
		return err
	}

	provisioningType, err := virtualmachine.GetDefaultDiskProvisioningType(
		vmCtx, vcClient, storageProfileID)
	if err != nil {
		reason, msg := errToConditionReasonAndMessage(err)
		pkgcnd.MarkFalse(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady, reason, "%s", msg)
		return err
	}

	createArgs.Storage = vmStorage
	createArgs.StorageProvisioning = provisioningType
	createArgs.StorageProfileID = storageProfileID
	createArgs.IsEncryptedStorageProfile = isEnc
	pkgcnd.MarkTrue(vmCtx.VM, vmopv1.VirtualMachineConditionStorageReady)

	return nil
}

func (vs *vSphereVMProvider) vmCreateDoNetworking(
	vmCtx pkgctx.VirtualMachineContext,
	vcClient *vcclient.Client,
	createArgs *VMCreateArgs) error {

	networkSpec := vmCtx.VM.Spec.Network
	if networkSpec == nil || networkSpec.Disabled {
		pkgcnd.Delete(vmCtx.VM, vmopv1.VirtualMachineConditionNetworkReady)
		return nil
	}

	results, err := network.CreateAndWaitForNetworkInterfaces(
		vmCtx,
		vs.k8sClient,
		vcClient.VimClient(),
		vcClient.Finder(),
		nil, // Don't know the CCR yet (needed to resolve backings for NSX-T)
		networkSpec)
	if err != nil {
		pkgcnd.MarkError(vmCtx.VM, vmopv1.VirtualMachineConditionNetworkReady, "NotReady", err)
		return err
	}

	createArgs.NetworkResults = results
	pkgcnd.MarkTrue(vmCtx.VM, vmopv1.VirtualMachineConditionNetworkReady)

	return nil
}

func (vs *vSphereVMProvider) vmCreateGenConfigSpec(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	// TODO: This is a partial dupe of what's done in the update path in the remaining Session code. I got
	// tired of trying to keep that in sync so we get to live with a frankenstein thing longer.

	var configSpec vimtypes.VirtualMachineConfigSpec
	if rawConfigSpec := createArgs.VMClass.Spec.ConfigSpec; len(rawConfigSpec) > 0 {
		vmClassConfigSpec, err := GetVMClassConfigSpec(vmCtx, rawConfigSpec)
		if err != nil {
			return err
		}
		configSpec = vmClassConfigSpec
	} else {
		configSpec = virtualmachine.ConfigSpecFromVMClassDevices(&createArgs.VMClass.Spec)
	}

	var minCPUFreq uint64
	if res := createArgs.VMClass.Spec.Policies.Resources; !res.Requests.Cpu.IsZero() || !res.Limits.Cpu.IsZero() {
		freq, err := vs.getOrComputeCPUMinFrequency(vmCtx)
		if err != nil {
			return err
		}
		minCPUFreq = freq
	}

	createArgs.ConfigSpec = virtualmachine.CreateConfigSpec(
		vmCtx,
		configSpec,
		createArgs.VMClass.Spec,
		createArgs.ImageStatus,
		minCPUFreq)

	if pkgcfg.FromContext(vmCtx).Features.FastDeploy {
		if err := vs.vmCreateGenConfigSpecImage(vmCtx, createArgs); err != nil {
			return err
		}
		createArgs.ConfigSpec.VmProfile = []vimtypes.BaseVirtualMachineProfileSpec{
			&vimtypes.VirtualMachineDefinedProfileSpec{
				ProfileId: createArgs.StorageProfileID,
			},
		}
	}

	// Get the encryption class details for the VM.
	if pkgcfg.FromContext(vmCtx).Features.BringYourOwnEncryptionKey {
		if err := vmconfcrypto.Reconcile(
			vmCtx,
			vs.k8sClient,
			vs.vcClient.VimClient(),
			vmCtx.VM,
			vmCtx.MoVM,
			&createArgs.ConfigSpec); err != nil {

			return err
		}
	}

	if err := vs.vmCreateGenConfigSpecExtraConfig(vmCtx, createArgs); err != nil {
		return err
	}

	if err := vs.vmCreateGenConfigSpecChangeBootDiskSize(vmCtx, createArgs); err != nil {
		return err
	}

	if err := vs.vmCreateGenConfigSpecZipNetworkInterfaces(vmCtx, createArgs); err != nil {
		return err
	}

	return nil
}

func (vs *vSphereVMProvider) vmCreateGenConfigSpecImage(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) (retErr error) {

	if createArgs.ImageStatus.Type != "OVF" {
		return nil
	}

	var (
		itemID      = createArgs.ImageStatus.ProviderItemID
		itemVersion = createArgs.ImageStatus.ProviderContentVersion
	)

	if itemID == "" {
		return errors.New("empty image provider item id")
	}
	if itemVersion == "" {
		return errors.New("empty image provider content version")
	}

	var (
		vmiCache    vmopv1.VirtualMachineImageCache
		vmiCacheKey = ctrlclient.ObjectKey{
			Namespace: pkgcfg.FromContext(vmCtx).PodNamespace,
			Name:      pkgutil.VMIName(itemID),
		}
	)

	// Get the VirtualMachineImageCache resource.
	if err := vs.k8sClient.Get(
		vmCtx,
		vmiCacheKey,
		&vmiCache); err != nil {

		return fmt.Errorf(
			"failed to get vmi cache object: %w, %w",
			err,
			pkgerr.VMICacheNotReadyError{Name: vmiCacheKey.Name})
	}

	// Check if the OVF data is ready.
	c := pkgcnd.Get(vmiCache, vmopv1.VirtualMachineImageCacheConditionOVFReady)
	switch {
	case c != nil && c.Status == metav1.ConditionFalse:

		return fmt.Errorf(
			"failed to get ovf: %s: %w",
			c.Message,
			pkgerr.VMICacheNotReadyError{Name: vmiCacheKey.Name})

	case c == nil,
		c.Status != metav1.ConditionTrue,
		vmiCache.Status.OVF == nil,
		vmiCache.Status.OVF.ProviderVersion != itemVersion:

		return pkgerr.VMICacheNotReadyError{
			Message: "cached ovf not ready",
			Name:    vmiCacheKey.Name,
		}
	}

	// Get the OVF data.
	var ovfConfigMap corev1.ConfigMap
	if err := vs.k8sClient.Get(
		vmCtx,
		ctrlclient.ObjectKey{
			Namespace: vmiCache.Namespace,
			Name:      vmiCache.Status.OVF.ConfigMapName,
		},
		&ovfConfigMap); err != nil {

		return fmt.Errorf(
			"failed to get ovf configmap: %w, %w",
			err,
			pkgerr.VMICacheNotReadyError{Name: vmiCacheKey.Name})
	}

	var ovfEnvelope ovf.Envelope
	if err := yaml.Unmarshal(
		[]byte(ovfConfigMap.Data["value"]), &ovfEnvelope); err != nil {

		return fmt.Errorf("failed to unmarshal ovf yaml into envelope: %w", err)
	}

	ovfConfigSpec, err := ovfEnvelope.ToConfigSpec()
	if err != nil {
		return fmt.Errorf("failed to transform ovf to config spec: %w", err)
	}

	if createArgs.ConfigSpec.GuestId == "" {
		createArgs.ConfigSpec.GuestId = ovfConfigSpec.GuestId
	}

	// Inherit the image's vAppConfig.
	createArgs.ConfigSpec.VAppConfig = ovfConfigSpec.VAppConfig

	// Merge the image's extra config.
	if srcEC := ovfConfigSpec.ExtraConfig; len(srcEC) > 0 {
		if dstEC := createArgs.ConfigSpec.ExtraConfig; len(dstEC) == 0 {
			// The current config spec doesn't have any extra config, so just
			// set it to use the extra config from the image.
			createArgs.ConfigSpec.ExtraConfig = srcEC
		} else {
			// The current config spec already has extra config, so merge the
			// extra config from the image, but do not overwrite any keys that
			// already exist in the current config spec.
			createArgs.ConfigSpec.ExtraConfig = object.OptionValueList(dstEC).
				Join(srcEC...)
		}
	}

	// Inherit the image's disks and their controllers.
	pkgutil.CopyStorageControllersAndDisks(
		&createArgs.ConfigSpec,
		ovfConfigSpec,
		createArgs.StorageProfileID)

	return nil
}

func (vs *vSphereVMProvider) vmCreateGenConfigSpecExtraConfig(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	ecMap := maps.Clone(vs.globalExtraConfig)

	if v, exists := ecMap[constants.ExtraConfigRunContainerKey]; exists {
		// The local-vcsim config sets the JSON_EXTRA_CONFIG with RUN.container so vcsim
		// creates a container for the VM. The only current use of this template function
		// is to fill in the {{.Name}} from the images status.
		renderTemplateFn := func(name, text string) string {
			t, err := template.New(name).Parse(text)
			if err != nil {
				return text
			}
			b := strings.Builder{}
			if err := t.Execute(&b, createArgs.ImageStatus); err != nil {
				return text
			}
			return b.String()
		}
		k := constants.ExtraConfigRunContainerKey
		ecMap[k] = renderTemplateFn(k, v)
	}

	if pkgutil.HasVirtualPCIPassthroughDeviceChange(createArgs.ConfigSpec.DeviceChange) {
		mmioSize := vmCtx.VM.Annotations[constants.PCIPassthruMMIOOverrideAnnotation]
		if mmioSize == "" {
			mmioSize = constants.PCIPassthruMMIOSizeDefault
		}
		if mmioSize != "0" {
			ecMap[constants.PCIPassthruMMIOExtraConfigKey] = constants.ExtraConfigTrue
			ecMap[constants.PCIPassthruMMIOSizeExtraConfigKey] = mmioSize
		}
	}

	// The ConfigSpec's current ExtraConfig values (that came from the class)
	// take precedence over what was set here.
	createArgs.ConfigSpec.ExtraConfig = pkgutil.OptionValues(
		createArgs.ConfigSpec.ExtraConfig).
		Append(pkgutil.OptionValuesFromMap(ecMap)...)

	// Leave constants.VMOperatorV1Alpha1ExtraConfigKey for the update path (if that's still even needed)

	return nil
}

func (vs *vSphereVMProvider) vmCreateGenConfigSpecChangeBootDiskSize(
	vmCtx pkgctx.VirtualMachineContext,
	_ *VMCreateArgs) error {

	advanced := vmCtx.VM.Spec.Advanced
	if advanced == nil || advanced.BootDiskCapacity == nil || advanced.BootDiskCapacity.IsZero() {
		return nil
	}

	// TODO: How to we determine the DeviceKey for the DeviceChange entry? We probably have to
	// crack the image/source, which is hard to do ATM. Punt on this for a placement consideration
	// and we'll resize the boot (first) disk after VM create like before.

	return nil
}

func (vs *vSphereVMProvider) vmCreateGenConfigSpecZipNetworkInterfaces(
	vmCtx pkgctx.VirtualMachineContext,
	createArgs *VMCreateArgs) error {

	if vmCtx.VM.Spec.Network == nil || vmCtx.VM.Spec.Network.Disabled {
		pkgutil.RemoveDevicesFromConfigSpec(&createArgs.ConfigSpec, pkgutil.IsEthernetCard)
		return nil
	}

	resultsIdx := 0
	var unmatchedEthDevices []int

	for idx := range createArgs.ConfigSpec.DeviceChange {
		spec := createArgs.ConfigSpec.DeviceChange[idx].GetVirtualDeviceConfigSpec()
		if spec == nil || !pkgutil.IsEthernetCard(spec.Device) {
			continue
		}

		device := spec.Device
		ethCard := device.(vimtypes.BaseVirtualEthernetCard).GetVirtualEthernetCard()

		if resultsIdx < len(createArgs.NetworkResults.Results) {
			err := network.ApplyInterfaceResultToVirtualEthCard(vmCtx, ethCard, &createArgs.NetworkResults.Results[resultsIdx])
			if err != nil {
				return err
			}
			resultsIdx++

		} else {
			// This ConfigSpec Ethernet device does not have a corresponding entry in the VM Spec, so we
			// won't ever have a backing for it. Remove it from the ConfigSpec since that is the easiest
			// thing to do, since extra NICs can cause later complications around GOSC and other customizations.
			// The downside with this is that if a NIC is added to the VM Spec, it won't necessarily have this
			// config but the default. Revisit this later if we don't like that behavior.
			unmatchedEthDevices = append(unmatchedEthDevices, idx-len(unmatchedEthDevices))
		}
	}

	if len(unmatchedEthDevices) > 0 {
		deviceChange := createArgs.ConfigSpec.DeviceChange
		for _, idx := range unmatchedEthDevices {
			deviceChange = append(deviceChange[:idx], deviceChange[idx+1:]...)
		}
		createArgs.ConfigSpec.DeviceChange = deviceChange
	}

	// Any remaining VM Spec network interfaces were not matched with a device in the ConfigSpec, so
	// create a default virtual ethernet card for them.
	for i := resultsIdx; i < len(createArgs.NetworkResults.Results); i++ {
		ethCardDev, err := network.CreateDefaultEthCard(vmCtx, &createArgs.NetworkResults.Results[i])
		if err != nil {
			return err
		}

		createArgs.ConfigSpec.DeviceChange = append(createArgs.ConfigSpec.DeviceChange, &vimtypes.VirtualDeviceConfigSpec{
			Operation: vimtypes.VirtualDeviceConfigSpecOperationAdd,
			Device:    ethCardDev,
		})
	}

	return nil
}

func (vs *vSphereVMProvider) vmUpdateGetArgs(
	vmCtx pkgctx.VirtualMachineContext) (*vmUpdateArgs, error) {

	updateArgs := &vmUpdateArgs{}

	vmClass, err := GetVirtualMachineClass(vmCtx, vs.k8sClient)
	if err != nil {
		return nil, err
	}
	updateArgs.VMClass = vmClass

	var resourcePolicy *vmopv1.VirtualMachineSetResourcePolicy
	if vmCtx.VM.Annotations[pkgconst.ClusterModuleNameAnnotationKey] != "" {
		var err error
		// Post create the resource policy is only needed to set the cluster module.
		resourcePolicy, err = GetVMSetResourcePolicy(vmCtx, vs.k8sClient)
		if err != nil {
			return nil, err
		}
		if resourcePolicy == nil {
			return nil, fmt.Errorf("cannot set cluster module without resource policy")
		}
	}
	updateArgs.ResourcePolicy = resourcePolicy

	bsData, err := GetVirtualMachineBootstrap(vmCtx, vs.k8sClient)
	if err != nil {
		return nil, err
	}
	updateArgs.BootstrapData = bsData

	ecMap := maps.Clone(vs.globalExtraConfig)
	maps.DeleteFunc(ecMap, func(k string, v string) bool {
		// Remove keys that we only want set on create.
		return k == constants.ExtraConfigRunContainerKey
	})
	updateArgs.ExtraConfig = ecMap

	if res := updateArgs.VMClass.Spec.Policies.Resources; !res.Requests.Cpu.IsZero() || !res.Limits.Cpu.IsZero() {
		freq, err := vs.getOrComputeCPUMinFrequency(vmCtx)
		if err != nil {
			return nil, err
		}
		updateArgs.MinCPUFreq = freq
	}

	var configSpec vimtypes.VirtualMachineConfigSpec
	if rawConfigSpec := updateArgs.VMClass.Spec.ConfigSpec; len(rawConfigSpec) > 0 {
		vmClassConfigSpec, err := GetVMClassConfigSpec(vmCtx, rawConfigSpec)
		if err != nil {
			return nil, err
		}
		configSpec = vmClassConfigSpec
	}

	updateArgs.ConfigSpec = virtualmachine.CreateConfigSpec(
		vmCtx,
		configSpec,
		updateArgs.VMClass.Spec,
		vmopv1.VirtualMachineImageStatus{},
		updateArgs.MinCPUFreq)

	return updateArgs, nil
}

func (vs *vSphereVMProvider) vmResizeGetArgs(
	vmCtx pkgctx.VirtualMachineContext) (*vmResizeArgs, error) {

	resizeArgs := &vmResizeArgs{}

	if !vmopv1util.IsClasslessVM(*vmCtx.VM) {
		vmClass, err := GetVirtualMachineClass(vmCtx, vs.k8sClient)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
		} else {
			resizeArgs.VMClass = &vmClass
		}
	}

	var resourcePolicy *vmopv1.VirtualMachineSetResourcePolicy
	if vmCtx.VM.Annotations[pkgconst.ClusterModuleNameAnnotationKey] != "" {
		var err error
		// Post create the resource policy is only needed to set the cluster module.
		resourcePolicy, err = GetVMSetResourcePolicy(vmCtx, vs.k8sClient)
		if err != nil {
			return nil, err
		}
		if resourcePolicy == nil {
			return nil, fmt.Errorf("cannot set cluster module without resource policy")
		}
	}
	resizeArgs.ResourcePolicy = resourcePolicy

	bsData, err := GetVirtualMachineBootstrap(vmCtx, vs.k8sClient)
	if err != nil {
		return nil, err
	}
	resizeArgs.BootstrapData = bsData

	if resizeArgs.VMClass != nil {
		var minCPUFreq uint64

		if res := resizeArgs.VMClass.Spec.Policies.Resources; !res.Requests.Cpu.IsZero() || !res.Limits.Cpu.IsZero() {
			freq, err := vs.getOrComputeCPUMinFrequency(vmCtx)
			if err != nil {
				return nil, err
			}
			minCPUFreq = freq
		}

		var configSpec vimtypes.VirtualMachineConfigSpec
		if rawConfigSpec := resizeArgs.VMClass.Spec.ConfigSpec; len(rawConfigSpec) > 0 {
			vmClassConfigSpec, err := GetVMClassConfigSpec(vmCtx, rawConfigSpec)
			if err != nil {
				return nil, err
			}
			configSpec = vmClassConfigSpec
		}

		resizeArgs.ConfigSpec = virtualmachine.CreateConfigSpec(
			vmCtx,
			configSpec,
			resizeArgs.VMClass.Spec,
			vmopv1.VirtualMachineImageStatus{},
			minCPUFreq)

	}

	return resizeArgs, nil
}
