// Copyright (c) 2024 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package watcher

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	backupapi "github.com/vmware-tanzu/vm-operator/pkg/backup/api"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
)

// DefaultWatchedPropertyPaths returns the default set of property paths to
// watch.
func DefaultWatchedPropertyPaths() []string {
	return []string{
		"config.extraConfig",
		"config.hardware.device",
		"config.keyId",
		"guest.ipStack",
		"guest.net",
		"summary.config.name",
		"summary.guest",
		"summary.overallStatus",
		"summary.runtime.host",
		"summary.runtime.powerState",
		"summary.storage.timestamp",
	}
}

const (
	extraConfigNamespacedNameKey     = "vmservice.namespacedName"
	taskType                         = "Task"
	virtualMachineType               = "VirtualMachine"
	configPropPath                   = "config"
	extraConfigPropPath              = configPropPath + ".extraConfig"
	extraConfigNamespaceNameKey      = "vmservice.namespacedName"
	extraConfigNamespaceNamePropPath = extraConfigPropPath + `["` + extraConfigNamespaceNameKey + `"]`
)

// defaultIgnoredExtraConfigKeys returns the default set of extra config keys to
// ignore.
var defaultIgnoredExtraConfigKeys = []string{
	backupapi.AdditionalResourcesYAMLExtraConfigKey,
	backupapi.BackupVersionExtraConfigKey,
	backupapi.ClassicDiskDataExtraConfigKey,
	backupapi.DisableAutoRegistrationExtraConfigKey,
	backupapi.EnableAutoRegistrationExtraConfigKey,
	backupapi.PVCDiskDataExtraConfigKey,
	backupapi.VMResourceYAMLExtraConfigKey,
	"govcsim",
	"guestinfo.metadata",
	"guestinfo.metadata.encoding",
	"guestinfo.userdata",
	"guestinfo.userdata.encoding",
	"guestinfo.vendordata",
	"guestinfo.vendordata.encoding",
	extraConfigNamespacedNameKey,
}

type moRef = vimtypes.ManagedObjectReference

type onUpdateCallbackFn func(context.Context, moRef, Update) (Result, bool, error)

type lookupNamespacedNameFn func(context.Context, moRef) (string, string, bool)

type Update struct {
	Kind    vimtypes.ObjectUpdateKind
	Changes []vimtypes.PropertyChange
}

type Result struct {
	// Ref is the ManagedObjectReference for the object that caused the update.
	Ref moRef

	// Namespace is the namespace to which the VirtualMachine resource belongs.
	// This field is set when the result is for a VirtualMachine or a Task for
	// a VirtualMachine.
	Namespace string

	// Name is the name of the VirtualMachine resource.
	// This field is set when the result is for a VirtualMachine or a Task for
	// a VirtualMachine.
	Name string

	// Verified is true if the VirtualMachine resource identified by Namespace
	// and Name has already been verified to exist in this Kubernetes cluster.
	// This field is set when the result is for a VirtualMachine or a Task for
	// a VirtualMachine.
	Verified bool

	// Update is the data that caused the watch result.
	Update Update
}

type Watcher struct {
	err        error
	errMu      sync.RWMutex
	cancel     func()
	chanDone   chan struct{}
	chanResult chan Result

	client *vim25.Client

	pc *property.Collector
	pf *property.Filter
	vm *view.Manager
	lv *view.ListView
	cv map[moRef]*view.ContainerView

	// refc is used to count the number of times a reference has been added to
	// the list view. If the Remove function is called on a reference with a
	// count of one, then the reference will be removed from the list view and
	// destroyed.
	refc map[moRef]map[string]struct{}

	ignoredExtraConfigKeys map[string]struct{}
	lookupNamespacedName   lookupNamespacedNameFn
}

// Done returns a channel that is closed when the watcher is shutdown.
func (w *Watcher) Done() <-chan struct{} {
	return w.chanDone
}

// Result returns a channel on which new results are received.
func (w *Watcher) Result() <-chan Result {
	return w.chanResult
}

// Err returns the error that caused the watcher to stop.
func (w *Watcher) Err() error {
	w.errMu.RLock()
	err := w.err
	w.errMu.RUnlock()
	return err
}

func (w *Watcher) setErr(err error) {
	w.errMu.Lock()
	w.err = err
	w.errMu.Unlock()
}

func newWatcher(
	ctx context.Context,
	client *vim25.Client,
	watchedPropertyPaths []string,
	additionalIgnoredExtraConfigKeys []string,
	lookupNamespacedName lookupNamespacedNameFn,
	refs []moRef,
	containerRefs []moRef) (*Watcher, error) {

	if watchedPropertyPaths == nil {
		watchedPropertyPaths = DefaultWatchedPropertyPaths()
	}
	ignoredExtraConfigKeys := slices.Concat(
		defaultIgnoredExtraConfigKeys,
		additionalIgnoredExtraConfigKeys)

	// Get the view manager.
	vm := view.NewManager(client)

	// For each container reference, create a container view.
	cvs, err := getContainerViews(ctx, vm, containerRefs)
	if err != nil {
		return nil, err
	}

	// Create a new list view used to monitor all of the provided references.
	// The references from the containerRefs parameter are monitored via their
	// container view.
	lv, err := vm.CreateListView(ctx, append(refs, toValueMoRefs(cvs)...))
	if err != nil {
		return nil, err
	}

	// Create a new property collector to watch for changes.
	pc, err := property.DefaultCollector(client).Create(ctx)
	if err != nil {
		return nil, err
	}

	// Create a new property filter that uses the list view created up above.
	pf, err := pc.CreateFilter(
		ctx,
		createPropFilter(lv.Reference(), watchedPropertyPaths))
	if err != nil {
		return nil, err
	}

	return &Watcher{
		chanDone:               make(chan struct{}),
		chanResult:             make(chan Result),
		client:                 client,
		pc:                     pc,
		pf:                     pf,
		vm:                     vm,
		lv:                     lv,
		cv:                     cvs,
		refc:                   getRefCounterMap(append(refs, containerRefs...)),
		ignoredExtraConfigKeys: toSet(ignoredExtraConfigKeys),
		lookupNamespacedName:   lookupNamespacedName,
	}, nil
}

func (w *Watcher) close() {
	w.cancel()
	close(w.chanDone)

	_ = w.pf.Destroy(context.Background())
	_ = w.pc.Destroy(context.Background())
	_ = w.lv.Destroy(context.Background())
	for _, cv := range w.cv {
		_ = cv.Destroy(context.Background())
	}
}

// Start begins watching a vSphere server for updates to the provided
// references.
// If watchedPropertyPaths is nil, DefaultWatchedPropertyPaths will be used.
// The containerRefs parameter may be used to start the watcher with an initial
// list of entities to watch.
func Start(
	ctx context.Context,
	client *vim25.Client,
	watchedPropertyPaths []string,
	additionalIgnoredExtraConfigKeys []string,
	lookupNamespacedName lookupNamespacedNameFn,
	refs []moRef,
	containerRefs []moRef) (*Watcher, error) {

	logger := logr.FromContextOrDiscard(ctx).WithName("vSphereWatcher")

	logger.Info("Starting watcher")

	w, err := newWatcher(
		ctx,
		client,
		watchedPropertyPaths,
		additionalIgnoredExtraConfigKeys,
		lookupNamespacedName,
		refs,
		containerRefs)
	if err != nil {
		return nil, err
	}

	// Update the context with this watcher.
	setContext(ctx, w)

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	w.cancel = cancel

	go func() {
		defer func() {
			// Remove this watcher from the context. While there is no watcher
			// in the context, calls to Add/Remove will fail.
			setContext(ctx, nil)

			w.close()
		}()

		if err := w.pc.WaitForUpdatesEx(
			ctx,
			&property.WaitOptions{},
			func(ou []vimtypes.ObjectUpdate) bool {
				return w.onUpdate(ctx, ou)
			}); err != nil {

			w.setErr(err)
		}
	}()

	return w, nil
}

func (w *Watcher) onUpdate(
	ctx context.Context,
	ou []vimtypes.ObjectUpdate) bool {

	logger := logr.FromContextOrDiscard(ctx)
	logger.V(4).Info("OnUpdate", "objectUpdates", ou)

	updates := map[moRef]Update{}

	for i := range ou {
		oui := ou[i]
		if oui.Kind != vimtypes.ObjectUpdateKindLeave {
			if v, ok := updates[oui.Obj]; !ok {
				updates[oui.Obj] = Update{
					Kind:    oui.Kind,
					Changes: oui.ChangeSet,
				}
			} else {
				v.Changes = append(v.Changes, oui.ChangeSet...)
				updates[oui.Obj] = v
			}
		}
	}

	for obj, update := range updates {

		var onUpdate onUpdateCallbackFn

		switch obj.Type {
		case virtualMachineType:
			onUpdate = w.onVirtualMachine
		case taskType:
			onUpdate = w.onTask
		default:
			onUpdate = w.onObject
		}

		result, ok, err := onUpdate(ctx, obj, update)
		if err != nil {
			w.setErr(err)
			return true
		}

		if ok {
			logger.V(4).Info("Sending result", "result", result)
			go func(r Result) { w.chanResult <- r }(result)
		}
	}

	return false
}

func (w *Watcher) onVirtualMachine(
	ctx context.Context,
	obj moRef,
	update Update) (Result, bool, error) {

	var (
		namespace string
		name      string
		verified  bool
	)

	// This update will be skipped if after removing all of the changes for
	// the ignoredExtraConfigKeys there is nothing left.
	var ignoredChanges int
	for i := range update.Changes {
		c := update.Changes[i]
		ignore := false
		if c.Name == extraConfigPropPath {
			if aov, ok := c.Val.(vimtypes.ArrayOfOptionValue); ok {
				ignore, namespace, name = checkExtraConfig(
					aov, w.ignoredExtraConfigKeys)
			}
		}
		if ignore {
			ignoredChanges++
		}
	}
	if ignoredChanges == len(update.Changes) {
		return Result{}, false, nil
	}

	if w.lookupNamespacedName != nil {
		if namespace == "" || name == "" || update.Kind == vimtypes.ObjectUpdateKindEnter {
			namespace, name, verified = w.lookupNamespacedName(ctx, obj)
		}
	}

	if update.Kind == vimtypes.ObjectUpdateKindEnter && verified {
		// The behavior of Controller-Runtime to sync all objects upon startup
		// will cause *existing* VMs to be reconciled. Therefore, do not emit a
		// result when the object is entering the scope of the watcher and the
		// corresponding Kubernetes object already exists with a matching
		// status.uniqueID field.
		return Result{}, false, nil
	}

	if namespace == "" || name == "" {
		var content []vimtypes.ObjectContent
		err := property.DefaultCollector(w.client).RetrieveOne(
			ctx,
			obj,
			[]string{extraConfigNamespaceNamePropPath},
			&content,
		)
		if err != nil {
			return Result{}, false, err
		}
		namespace, name = namespacedNameFromObjContent(content)
	}

	if namespace != "" && name != "" {
		return Result{
			Namespace: namespace,
			Name:      name,
			Ref:       obj,
			Verified:  verified,
			Update:    update,
		}, true, nil
	}

	return Result{}, false, nil
}

func (w *Watcher) onTask(
	ctx context.Context,
	obj moRef,
	update Update) (Result, bool, error) {

	if update.Kind == vimtypes.ObjectUpdateKindEnter {
		return Result{}, false, nil
	}

	var ti *vimtypes.TaskInfo
	for i := range update.Changes {
		c := update.Changes[i]
		if c.Name == "info" {
			ti = ptr.To(c.Val.(vimtypes.TaskInfo))
			break
		}
	}

	if ti == nil {
		return Result{}, false, nil
	}

	fmt.Printf("\n\ntask.info=%[1]T, %+[1]v\n\n", ti)

	return Result{
		Ref: obj,
		//Update: update,
	}, true, nil
}

func (w *Watcher) onObject(
	ctx context.Context,
	obj moRef,
	update Update) (Result, bool, error) {

	return Result{}, false, nil
}

func checkExtraConfig(
	aov vimtypes.ArrayOfOptionValue,
	ignoredKeys map[string]struct{}) (ignore bool, namespace, name string) {

	hasNonIgnoredKey := false

	for j := range aov.OptionValue {
		if ov := aov.OptionValue[j].GetOptionValue(); ov != nil {
			// Get the namespace and name of the VM from the changes
			// if they are present there.
			if ov.Key == extraConfigNamespacedNameKey {
				if namespace == "" || name == "" {
					if s, ok := ov.Value.(string); ok {
						namespace, name = namespacedNameFromString(s)
					}
				}
			}
			// Note if the key cannot be ignored.
			if _, ok := ignoredKeys[ov.Key]; !ok {
				hasNonIgnoredKey = true
			}
			if hasNonIgnoredKey && (namespace != "" && name != "") {
				break
			}
		}
	}

	return !hasNonIgnoredKey, namespace, name
}

func (w *Watcher) add(
	ctx context.Context,
	ref moRef,
	asContainer bool,
	id string) error {

	if _, ok := w.refc[ref]; ok {
		w.refc[ref][id] = struct{}{}
		return nil
	}

	if asContainer {
		cv, err := w.vm.CreateContainerView(
			ctx,
			ref,
			[]string{virtualMachineType},
			true)
		if err != nil {
			return err
		}

		if _, err := w.lv.Add(
			ctx,
			[]vimtypes.ManagedObjectReference{cv.Reference()}); err != nil {

			if err2 := cv.Destroy(context.Background()); err2 != nil {
				return fmt.Errorf(
					"failed to destroy container view after adding "+
						"it to list failed: addErr=%w, destroyErr=%w", err, err2)
			}

			return err
		}

		w.cv[ref] = cv
	} else if _, err := w.lv.Add(
		ctx,
		[]vimtypes.ManagedObjectReference{ref}); err != nil {

		return err
	}

	if w.refc[ref] == nil {
		w.refc[ref] = map[string]struct{}{}
	}
	w.refc[ref][id] = struct{}{}

	return nil
}

func (w *Watcher) remove(_ context.Context, ref moRef, id string) error {
	if _, ok := w.refc[ref]; !ok {
		return nil
	}

	// Only remove the object from the list view if it has a ref count of
	// one.
	if len(w.refc[ref]) > 1 {
		delete(w.refc[ref], id)
		return nil
	}

	if cv, ok := w.cv[ref]; ok {
		_, err := w.lv.Remove(context.Background(), []moRef{cv.Reference()})
		if err != nil {
			return err
		}

		if err := cv.Destroy(context.Background()); err != nil {
			return err
		}

		delete(w.cv, ref)
	}

	delete(w.refc[ref], id)
	return nil
}

func getContainerViews(
	ctx context.Context,
	vm *view.Manager,
	refs []moRef) (map[moRef]*view.ContainerView, error) {

	cvMap := map[moRef]*view.ContainerView{}

	if len(refs) == 0 {
		return cvMap, nil
	}

	var resultErr error
	for i := range refs {
		r := refs[i]

		if _, ok := cvMap[r]; ok {
			// Ignore duplicates.
			continue
		}

		cv, err := vm.CreateContainerView(
			ctx,
			r,
			[]string{virtualMachineType},
			true)
		if err != nil {
			resultErr = err
			break
		}
		cvMap[r] = cv
	}

	if resultErr != nil {
		// There was an error creating container views, so make sure to clean up
		// any views that *were* created before returning.
		for _, cv := range cvMap {
			if err := cv.Destroy(context.Background()); err != nil {
				resultErr = fmt.Errorf("%w,%w", resultErr, err)
			}
		}
		return nil, resultErr
	}

	return cvMap, nil
}

func getRefCounterMap(refs []moRef) map[moRef]map[string]struct{} {
	refCounter := map[moRef]map[string]struct{}{}
	if len(refs) == 0 {
		return refCounter
	}
	for i := range refs {
		if _, ok := refCounter[refs[i]]; ok {
			// Ignore duplicates.
			continue
		}
		refCounter[refs[i]] = map[string]struct{}{}
	}
	return refCounter
}

func namespacedNameFromString(s string) (string, string) {
	if p := strings.Split(s, "/"); len(p) == 2 {
		return p[0], p[1]
	}
	return "", ""
}

func namespacedNameFromObjContent(
	oc []vimtypes.ObjectContent) (string, string) {

	for i := range oc {
		for j := range oc[i].PropSet {
			dp := oc[i].PropSet[j]
			if dp.Name == extraConfigNamespaceNamePropPath {
				if ov, ok := dp.Val.(vimtypes.OptionValue); ok {
					if v, ok := ov.Value.(string); ok {
						return namespacedNameFromString(v)
					}
				}
			}
		}
	}
	return "", ""
}

func createPropFilter(ref moRef, watchedPropertyPaths []string) vimtypes.CreateFilter {
	return vimtypes.CreateFilter{
		Spec: vimtypes.PropertyFilterSpec{
			ObjectSet: []vimtypes.ObjectSpec{
				{
					Obj:  ref,
					Skip: &[]bool{true}[0],
					SelectSet: []vimtypes.BaseSelectionSpec{
						&vimtypes.TraversalSpec{
							Type: "ListView",
							Path: "view",
							SelectSet: []vimtypes.BaseSelectionSpec{
								// ListView --> ContainerView
								&vimtypes.SelectionSpec{
									Name: "visitViews",
								},
							},
						},
						// ContainerView --> VM
						&vimtypes.TraversalSpec{
							SelectionSpec: vimtypes.SelectionSpec{
								Name: "visitViews",
							},
							Type: "ContainerView",
							Path: "view",
						},
					},
				},
			},
			PropSet: []vimtypes.PropertySpec{
				{
					Type:    virtualMachineType,
					PathSet: watchedPropertyPaths,
				},
				{
					Type:    taskType,
					PathSet: []string{"info"},
				},
			},
		},
	}
}

type hasRef interface {
	Reference() moRef
}

// toValueMoRefs returns a list of references from the values in the
// provided map.
func toValueMoRefs[M ~map[K]V, K comparable, V hasRef](m M) []moRef {
	if len(m) == 0 {
		return nil
	}
	r := make([]moRef, 0, len(m))
	for _, v := range m {
		r = append(r, v.Reference())
	}
	return r
}

func toSet[K comparable](s []K) map[K]struct{} {
	if len(s) == 0 {
		return nil
	}
	r := make(map[K]struct{}, len(s))
	for i := range s {
		r[s[i]] = struct{}{}
	}
	return r
}
