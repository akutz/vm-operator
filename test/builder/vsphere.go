// Copyright (c) 2023 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package builder

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	vapirest "github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/api/v1alpha2"
	"github.com/vmware-tanzu/vm-operator/api/v1alpha2/common"
	topologyv1 "github.com/vmware-tanzu/vm-operator/external/tanzu-topology/api/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/pkg/lib"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	"github.com/vmware-tanzu/vm-operator/test/testutil"
)

type NetworkEnv string

const (
	NetworkEnvVDS   = NetworkEnv("vds")
	NetworkEnvNSXT  = NetworkEnv("nsx-t")
	NetworkEnvNamed = NetworkEnv("named")

	NsxTLogicalSwitchUUID = "nsxt-dummy-ls-uuid"

	// zoneCountForHA is how many zones to create for HA.
	zoneCountForHA = 3

	// clustersPerZone is how many clusters to create per zone.
	clustersPerZone = 1
)

type VSphereOptions struct {
	// VCSim may be used to enable / configure vC Simulator.
	VCSim VCSimOptions

	// When WithContentLibrary is true
	ContentLibraryID                    string
	ContentLibraryItemIDForLinuxImage   string
	ContentLibraryItemIDForWindowsImage string

	// When WithoutStorageClass is false
	StorageClassName string
	StorageProfileID string

	NetworkEnv NetworkEnv

	Host       string
	User       string
	Pass       string
	Datacenter string
	Datastore  string
	Folder     string
	Network    string

	// Pool is used when fault domains are disabled.
	Pool string

	// Zones is used when fault domains are enabled.
	Zones []VSphereZoneOptions

	// WithJSONExtraConfig enables additional ExtraConfig that is included when
	// creating a VM.
	WithJSONExtraConfig string
}

type VSphereZoneOptions struct {
	Name  string
	Pools []string
}

type VCSimOptions struct {
	// Enabled indicates vC Sim should be used.
	Enabled bool

	// The following properties are used when fault domains are enabled.
	NumFaultDomains int
	ZoneCount       int
	ClustersPerZone int
}

type vsphereConfig struct {
	opts   VSphereOptions
	fssMap map[string]bool
	vcsim  vcsimConfig

	serverURL *url.URL

	client     *govmomi.Client
	restClient *vapirest.Client
	recorder   record.Recorder

	datacenter *object.Datacenter
	datastore  *object.Datastore
	finder     *find.Finder
	folder     *object.Folder
	network    object.NetworkReference

	// pool is used when fault domains are disabled
	pool *object.ResourcePool

	// zones are used when fault domains are enabled
	zones []vsphereZoneConfig
}

type vcsimConfig struct {
	model  *simulator.Model
	server *simulator.Server

	certDir     string
	pki         pkiToolchain
	tlsCertPath string
	tlsKeyPath  string
}

type vsphereZoneConfig struct {
	name  string
	pools []*object.ResourcePool
}

func (c *vsphereConfig) init(
	ctx context.Context,
	klient ctrlclient.Client,
	podNamespace string) error {

	if c.opts.VCSim.Enabled {
		if err := c.initVCSim(ctx); err != nil {
			return err
		}
	}

	if err := c.initEnv(podNamespace); err != nil {
		return err
	}

	if err := c.initInventory(ctx); err != nil {
		return err
	}

	if err := c.initConfigMapAndCredentialSecret(
		ctx, klient, podNamespace); err != nil {
		return err
	}

	if err := c.initZones(ctx, klient); err != nil {
		return err
	}

	if c.opts.VCSim.Enabled {
		if err := c.initVCSimContentLibrary(ctx); err != nil {
			return err
		}
	}

	if err := c.initImageRegistry(ctx, klient); err != nil {
		return err
	}

	if ok := c.fssMap[lib.NamespacedVMClassFSS]; !ok {
		if err := c.initDefaultVMClasses(ctx, klient); err != nil {
			return err
		}
	}

	return nil
}

func (c *vsphereConfig) initInventory(ctx context.Context) error {
	if c.opts.Pool != "" && len(c.opts.Zones) > 0 {
		return fmt.Errorf("root resource pool and zones are both non-empty")
	}

	serverURL, err := url.Parse(c.opts.Host)
	if err != nil {
		return err
	}

	if u := c.opts.User; u != "" {
		serverURL.User = url.UserPassword(u, c.opts.Pass)
	}

	c.serverURL = serverURL

	vimClient, err := govmomi.NewClient(ctx, serverURL, true)
	if err != nil {
		return err
	}
	c.client = vimClient

	c.restClient = vapirest.NewClient(c.client.Client)
	if err := c.restClient.Login(ctx, serverURL.User); err != nil {
		return err
	}

	c.finder = find.NewFinder(c.client.Client)

	// Find the datacenter with its inventory path.
	datacenter, err := c.finder.Datacenter(ctx, c.opts.Datacenter)
	if err != nil {
		return err
	}
	c.datacenter = datacenter
	c.finder.SetDatacenter(c.datacenter)

	// Find the datastore with its inventory path.
	datastore, err := c.finder.Datastore(ctx, c.opts.Datastore)
	if err != nil {
		return err
	}
	c.datastore = datastore

	// Find the root folder with its inventory path.
	folder, err := c.finder.Folder(ctx, c.opts.Folder)
	if err != nil {
		return err
	}
	c.folder = folder

	// Find the default network with its inventory path.
	network, err := c.finder.Network(ctx, c.opts.Network)
	if err != nil {
		return err
	}
	c.network = network

	// Find the root resource pool with its inventory path if one was provided.
	if p := c.opts.Pool; p != "" {
		pool, err := c.finder.ResourcePool(ctx, p)
		if err != nil {
			return err
		}
		c.pool = pool
	}

	// Find the resource pools for the zones if there are any.
	c.zones = make([]vsphereZoneConfig, len(c.opts.Zones))
	for i := range c.opts.Zones {
		zoneOpts := c.opts.Zones[i]

		c.zones[i].name = zoneOpts.Name
		c.zones[i].pools = make([]*object.ResourcePool, len(zoneOpts.Pools))

		for j := range zoneOpts.Pools {
			pool, err := c.finder.ResourcePool(ctx, zoneOpts.Pools[j])
			if err != nil {
				return err
			}
			c.zones[i].pools[j] = pool
		}
	}

	return nil
}

func (c *vsphereConfig) initVCSim(ctx context.Context) error {

	// Create a temp directory for the certs needed for vC Sim.
	certDir, err := os.MkdirTemp(os.TempDir(), "")
	if err != nil {
		return err
	}
	c.vcsim.certDir = certDir

	// Generate the certs for vC Sim.
	pki, err := generatePKIToolchain()
	if err != nil {
		return err
	}
	c.vcsim.pki = pki

	// Write the CA pub key and cert pub and private keys to the cert dir.
	c.vcsim.tlsCertPath = path.Join(c.vcsim.certDir, "tls.crt")
	c.vcsim.tlsKeyPath = path.Join(c.vcsim.certDir, "tls.key")
	if err := os.WriteFile(
		c.vcsim.tlsCertPath,
		c.vcsim.pki.publicKeyPEM,
		0400); err != nil {

		return err
	}
	if err := os.WriteFile(
		c.vcsim.tlsKeyPath,
		c.vcsim.pki.privateKeyPEM,
		0400); err != nil {

		return err
	}
	var tlsCert tls.Certificate
	if tlsCert, err = tls.LoadX509KeyPair(
		c.vcsim.tlsCertPath, c.vcsim.tlsKeyPath); err != nil {

		return err
	}

	vcModel := simulator.VPX()

	// This ensures no stand-alone hosts are created outside of any cluster.
	//
	// By Default, the Model being used by vcsim has two ResourcePools (one for
	// the cluster and host each). Setting Model.Host=0 ensures we only have one
	// ResourcePool, making it easier to pick the ResourcePool without having to
	// look up using a hardcoded path.
	vcModel.Host = 0

	// Always use three hosts in a cluster.
	vcModel.ClusterHost = 3

	// The number of clusters is determined by whether or not fault domains are
	// enabled.
	if ok := c.fssMap[lib.WcpFaultDomainsFSS]; !ok {
		vcModel.Cluster = 1
	} else {
		if c.opts.VCSim.NumFaultDomains > 0 {
			c.opts.VCSim.ZoneCount = c.opts.VCSim.NumFaultDomains
		} else if c.opts.VCSim.ZoneCount == 0 {
			c.opts.VCSim.ZoneCount = zoneCountForHA
		}
		if c.opts.VCSim.ClustersPerZone == 0 {
			c.opts.VCSim.ClustersPerZone = clustersPerZone
		}
		vcModel.Cluster = c.opts.VCSim.ZoneCount * c.opts.VCSim.ClustersPerZone
	}

	// Instantiate a new simulator from the model.
	if err := vcModel.Create(); err != nil {
		return err
	}

	vcModel.Service.RegisterEndpoints = true
	vcModel.Service.TLS = &tls.Config{
		Certificates: []tls.Certificate{
			tlsCert,
		},
		PreferServerCipherSuites: true,
		MinVersion:               tls.VersionTLS13,
	}

	c.vcsim.model = vcModel
	c.vcsim.server = c.vcsim.model.Service.NewServer()

	c.opts.Host = c.vcsim.server.URL.String()
	c.opts.User = simulator.DefaultLogin.Username()
	c.opts.Pass, _ = simulator.DefaultLogin.Password()

	c.opts.Datacenter = "/DC0"
	c.opts.Datastore = "/DC0/datastore/LocalDS_0"
	c.opts.Folder = "/DC0/vm"
	c.opts.Network = "/DC0/network/DC0_DVPG0"
	c.opts.StorageClassName = "vcsim-default-storageclass"
	c.opts.StorageProfileID = "aa6d5a82-1c88-45da-85d3-3d74b91a5bad"

	if ok := c.fssMap[lib.WcpFaultDomainsFSS]; !ok {
		// If fault domains are not enabled, then set the root resource pool to
		// the one cluster's "Resources" resource pool.
		c.opts.Pool = "/DC0/host/DC0_C0/Resources"
	} else {
		// Otherwise, use the resource pools from the clusters in each zone.
		k := 0
		c.opts.Zones = make([]VSphereZoneOptions, c.opts.VCSim.ZoneCount)
		for i := range c.opts.Zones {
			c.opts.Zones[i] = VSphereZoneOptions{
				Name:  fmt.Sprintf("zone-%d", i),
				Pools: make([]string, c.opts.VCSim.ClustersPerZone),
			}
			for j := range c.opts.Zones[i].Pools {
				c.opts.Zones[i].Pools[j] = fmt.Sprintf("/DC0/host/DC0_C%d/Resources", k)
				k++
			}
		}
	}

	if ok := c.fssMap[lib.InstanceStorageFSS]; ok {
		// Because of the way CSI works, instance storage needs the hosts'
		// FQDNs to be initialized.
		systems := simulator.Map.AllReference("HostNetworkSystem")
		for _, s := range systems {
			if h, ok := s.(*simulator.HostNetworkSystem); ok && h.Host != nil {
				h.DnsConfig = &types.HostDnsConfig{
					HostName:   h.Host.Reference().Value,
					DomainName: "vmop.vmware.com",
				}
			}
		}
	}

	// Configure the networking.
	switch c.opts.NetworkEnv {
	case NetworkEnvVDS:
		// Nothing more needed for VDS.
	case NetworkEnvNSXT:
		// Assign the DVPG's logical switch UUID.
		client, err := govmomi.NewClient(ctx, c.vcsim.server.URL, true)
		if err != nil {
			return err
		}
		finder := find.NewFinder(client.Client)
		datacenter, err := finder.Datacenter(ctx, c.opts.Datacenter)
		if err != nil {
			return err
		}
		finder.SetDatacenter(datacenter)
		network, err := finder.Network(ctx, c.opts.Network)
		if err != nil {
			return err
		}
		if dvpg, ok := simulator.Map.Get(
			network.Reference()).(*simulator.DistributedVirtualPortgroup); ok {

			dvpg.Config.LogicalSwitchUuid = NsxTLogicalSwitchUUID
			dvpg.Config.BackingType = "nsx"
		}
	}

	return nil
}

func (c *vsphereConfig) initZones(
	ctx context.Context,
	klient ctrlclient.Client) error {

	for i := range c.zones {
		// Create the zone.
		var az topologyv1.AvailabilityZone
		az.Name = c.zones[i].name

		// Get a list of the unique cluster MoIDs for the pools that are
		// part of this zone.
		clusterMoIDs := map[string]struct{}{}
		for j := range c.zones[i].pools {
			owner, err := c.zones[i].pools[j].Owner(ctx)
			if err != nil {
				return err
			}
			if _, ok := clusterMoIDs[owner.Reference().Value]; !ok {
				clusterMoIDs[owner.Reference().Value] = struct{}{}
			}
		}
		az.Spec.ClusterComputeResourceMoIDs = make([]string, len(clusterMoIDs))
		ccrMoIdIdx := 0
		for k := range clusterMoIDs {
			az.Spec.ClusterComputeResourceMoIDs[ccrMoIdIdx] = k
			ccrMoIdIdx++
		}

		// Create the zone resource in Kubernetes.
		if err := klient.Create(ctx, &az); err != nil {
			return err
		}
	}

	return nil
}

func (c *vsphereConfig) initConfigMapAndCredentialSecret(
	ctx context.Context,
	klient ctrlclient.Client,
	podNamespace string) error {

	password, _ := c.serverURL.User.Password()
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "vmop-credentials-",
			Namespace:    podNamespace,
		},
		Data: map[string][]byte{
			"username": []byte(c.serverURL.User.Username()),
			"password": []byte(password),
		},
	}

	if err := klient.Create(ctx, &secret); err != nil {
		return err
	}

	var vmopConfigMap corev1.ConfigMap
	vmopConfigMap.Name = "vsphere.provider.config.vmoperator.vmware.com"
	vmopConfigMap.Namespace = podNamespace

	vmopConfigMap.Data = map[string]string{
		"VcPNID":            c.serverURL.Hostname(),
		"VcPort":            c.serverURL.Port(),
		"VcCredsSecretName": secret.Name,
		"Datacenter":        c.datacenter.Reference().Value,
	}

	if c.opts.VCSim.Enabled {
		vmopConfigMap.Data["CAFilePath"] = c.vcsim.tlsCertPath
		vmopConfigMap.Data["InsecureSkipTLSVerify"] = "false"
	} else {
		vmopConfigMap.Data["InsecureSkipTLSVerify"] = "true"
	}

	if c.pool != nil {
		vmopConfigMap.Data["ResourcePool"] = c.pool.Reference().Value
	}

	if c.opts.StorageProfileID == "" {
		vmopConfigMap.Data["Datastore"] = c.datastore.Name()
		vmopConfigMap.Data["StorageClassRequired"] = "false"
	} else {
		vmopConfigMap.Data["StorageClassRequired"] = "true"
		storageClass := storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: c.opts.StorageClassName,
			},
			Parameters: map[string]string{
				"storagePolicyID": c.opts.StorageProfileID,
			},
			Provisioner: "fake",
		}
		if err := klient.Create(ctx, &storageClass); err != nil {
			return err
		}
	}

	// Create the VM Operator config map resource.
	if err := klient.Create(ctx, &vmopConfigMap); err != nil {
		return err
	}

	// Create the network config map resource as well.
	if err := klient.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vmoperator-network-config",
			Namespace: podNamespace,
		},
		Data: map[string]string{
			"nameservers": "1.1.1.1 1.0.0.1",
		},
	}); err != nil {

		return err
	}

	return nil
}

func (c *vsphereConfig) createWorkloadNamespace(
	ctx context.Context,
	klient ctrlclient.Client,
	namespace string) (string, *object.Folder, error) {

	var ns corev1.Namespace

	if namespace == "" {
		// If there was no namespace specified then create one.
		ns.GenerateName = "workload-"
		if err := klient.Create(ctx, &ns); err != nil {
			return "", nil, err
		}
	} else {
		// If a namespace is specified then make sure it exists.
		nsKey := client.ObjectKey{Name: namespace}
		if err := klient.Get(ctx, nsKey, &ns); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", nil, err
			}
			// The namespace was not found, so create it.
			ns.Name = namespace
			if err := klient.Create(ctx, &ns); err != nil {
				return "", nil, err
			}
		}
	}

	// Create the folder in the vSphere inventory for the namespace.
	nsFolder, err := c.folder.CreateFolder(ctx, ns.Name)
	if err != nil {
		return "", nil, err
	}

	if c.pool != nil {
		// Create the single resource pool for the namespace.
		nsPool, err := c.pool.Create(ctx, ns.Name, types.DefaultResourceConfigSpec())
		if err != nil {
			return "", nil, err
		}

		// Update the Kubernetes namespace annotations so they include the refs
		// to the vSphere folder and resource pool for the namespace.
		ns.Annotations = map[string]string{
			"vmware-system-vm-folder":     nsFolder.Reference().Value,
			"vmware-system-resource-pool": nsPool.Reference().Value,
		}
		if err := klient.Update(ctx, &ns); err != nil {
			return "", nil, err
		}
	} else {
		// Create the resource pools for the namespace in each zone.
		for i := range c.zones {
			nsInfo := topologyv1.NamespaceInfo{
				FolderMoId: nsFolder.Reference().Value,
			}

			// Create the resource pool for the namespace in each of the zone's
			// resource pools.
			for j := range c.zones[i].pools {
				pool := c.zones[i].pools[j]
				nsPool, err := pool.Create(
					ctx, ns.Name, types.DefaultResourceConfigSpec())
				if err != nil {
					return "", nil, err
				}
				nsInfo.PoolMoIDs = append(
					nsInfo.PoolMoIDs, nsPool.Reference().Value)
			}

			// Update the availability zone with the new namespace information.
			var az topologyv1.AvailabilityZone
			if err := klient.Get(
				ctx,
				client.ObjectKey{Name: c.zones[i].name},
				&az); err != nil {

				return "", nil, err
			}
			if az.Spec.Namespaces == nil {
				az.Spec.Namespaces = map[string]topologyv1.NamespaceInfo{}
			}
			az.Spec.Namespaces[ns.Name] = nsInfo
			if err := klient.Update(ctx, &az); err != nil {
				return "", nil, err
			}
		}
	}

	// If v1a2 is not enable then the ContentSource binding has to be created
	// in the new namespace.
	if ok := c.fssMap[lib.VMServiceV1Alpha2FSS]; !ok {
		if libraryID := c.opts.ContentLibraryID; libraryID != "" {
			csBinding := v1alpha1.ContentSourceBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      libraryID,
					Namespace: ns.Name,
				},
				ContentSourceRef: v1alpha1.ContentSourceReference{
					APIVersion: v1alpha1.SchemeGroupVersion.Group,
					Kind:       "ContentSource",
					Name:       libraryID,
				},
			}
			if err := klient.Create(ctx, &csBinding); err != nil {
				return "", nil, err
			}
		}
	}

	// Create a ResourceQuota resource in the new namespace that points to the
	// StorageClass.
	resourceQuota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "resource-quota-",
			Namespace:    ns.Name,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceName(
					c.opts.StorageClassName + ".storageclass.storage.k8s.io/persistentvolumeclaims",
				): resource.MustParse("1"),
			},
		},
	}
	if err := klient.Create(ctx, &resourceQuota); err != nil {
		return "", nil, err
	}

	// Create either a default class or binding pointing to one.
	if c.fssMap[lib.VMServiceV1Alpha2FSS] {
		vmClass := v1alpha2.VirtualMachineClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "best-effort-small",
				Namespace: ns.Name,
			},
			Spec: v1alpha2.VirtualMachineClassSpec{
				Hardware: v1alpha2.VirtualMachineClassHardware{
					Cpus:   int64(2),
					Memory: resource.MustParse("4Gi"),
				},
				Policies: v1alpha2.VirtualMachineClassPolicies{
					Resources: v1alpha2.VirtualMachineClassResources{
						Requests: v1alpha2.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("1Gi"),
							Memory: resource.MustParse("2Gi"),
						},
						Limits: v1alpha2.VirtualMachineResourceSpec{
							Cpu:    resource.MustParse("2Gi"),
							Memory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		}
		if err := klient.Create(ctx, &vmClass); err != nil {
			return "", nil, err
		}
	} else {
		if c.fssMap[lib.NamespacedVMClassFSS] {
			vmClass := v1alpha1.VirtualMachineClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "best-effort-small",
					Namespace: ns.Name,
				},
				Spec: v1alpha1.VirtualMachineClassSpec{
					Hardware: v1alpha1.VirtualMachineClassHardware{
						Cpus:   int64(2),
						Memory: resource.MustParse("4Gi"),
					},
					Policies: v1alpha1.VirtualMachineClassPolicies{
						Resources: v1alpha1.VirtualMachineClassResources{
							Requests: v1alpha1.VirtualMachineResourceSpec{
								Cpu:    resource.MustParse("1Gi"),
								Memory: resource.MustParse("2Gi"),
							},
							Limits: v1alpha1.VirtualMachineResourceSpec{
								Cpu:    resource.MustParse("2Gi"),
								Memory: resource.MustParse("4Gi"),
							},
						},
					},
				},
			}
			if err := klient.Create(ctx, &vmClass); err != nil {
				return "", nil, err
			}
		} else {
			vmClassBinding := v1alpha1.VirtualMachineClassBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "best-effort-small",
					Namespace: ns.Name,
				},
				ClassRef: v1alpha1.ClassReference{
					APIVersion: "vmoperator.vmware.com/v1alpha1",
					Kind:       "VirtualMachineClass",
					Name:       "best-effort-small",
				},
			}
			if err := klient.Create(ctx, &vmClassBinding); err != nil {
				return "", nil, err
			}
		}
	}

	// Make trip through the Finder to populate InventoryPath.
	objRef, err := c.finder.ObjectReference(ctx, nsFolder.Reference())
	if err != nil {
		return "", nil, err
	}

	return ns.Name, objRef.(*object.Folder), nil
}

func (c *vsphereConfig) destroyWorkloadNamespace(
	ctx context.Context,
	klient ctrlclient.Client,
	namespace string) error {

	var ns corev1.Namespace
	ns.Name = namespace

	// Try to delete the namespace resource (it may already be gone).
	if err := klient.Delete(ctx, &ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	// Delete the namespace folder.
	nsFolder, err := c.finder.Folder(ctx, c.folder.InventoryPath+"/"+ns.Name)
	if err != nil {
		return err
	}
	destroyFolderTask, err := nsFolder.Destroy(ctx)
	if err != nil {
		return err
	}
	destroyFolderTaskInfo, err := destroyFolderTask.WaitForResult(ctx)
	if err != nil {
		return err
	}
	if destroyFolderTaskInfo.Error != nil {
		return fmt.Errorf(destroyFolderTaskInfo.Error.LocalizedMessage)
	}

	if c.pool != nil {
		// Delete the namespace resource pool.
		nsPool, err := c.finder.ResourcePool(
			ctx, c.pool.InventoryPath+"/"+ns.Name)
		if err != nil {
			return err
		}
		destroyPoolTask, err := nsPool.Destroy(ctx)
		if err != nil {
			return err
		}
		destroyPoolTaskInfo, err := destroyPoolTask.WaitForResult(ctx)
		if err != nil {
			return err
		}
		if destroyPoolTaskInfo.Error != nil {
			return fmt.Errorf(destroyPoolTaskInfo.Error.LocalizedMessage)
		}
	} else {
		// Remove the namespace from each of the zones if they exist.
		for i := range c.zones {
			// Get the zone.
			var az topologyv1.AvailabilityZone
			if err := klient.Get(
				ctx,
				client.ObjectKey{Name: c.zones[i].name},
				&az); err != nil {

				return err
			}

			// If the zone has no namespaces, skip to the next zone.
			if len(az.Spec.Namespaces) == 0 {
				continue
			}

			// If the zone has the namespace, delete it, and update the zone.
			if _, ok := az.Spec.Namespaces[ns.Name]; ok {
				delete(az.Spec.Namespaces, ns.Name)
				if err := klient.Update(ctx, &az); err != nil {
					return err
				}
			}

			// Delete the resource pools for the namespace.
			for j := range c.zones[i].pools {
				nsPool, err := c.finder.ResourcePool(
					ctx, c.zones[i].pools[j].InventoryPath+"/"+ns.Name)
				if err != nil {
					return err
				}
				destroyPoolTask, err := nsPool.Destroy(ctx)
				if err != nil {
					return err
				}
				destroyPoolTaskInfo, err := destroyPoolTask.WaitForResult(ctx)
				if err != nil {
					return err
				}
				if destroyPoolTaskInfo.Error != nil {
					return fmt.Errorf(destroyPoolTaskInfo.Error.LocalizedMessage)
				}
			}
		}
	}

	return nil
}

func (c *vsphereConfig) initEnv(podNamespace string) error {

	// Let the vSphere provider known in which namespace the ConfigMap
	// responsible for configuring the provider can be found.
	if err := os.Setenv(lib.VmopNamespaceEnv, podNamespace); err != nil {
		return err
	}

	// Assume content library is used.
	if err := os.Setenv("CONTENT_API_WAIT_SECS", "1"); err != nil {
		return err
	}

	// Configure the networking model.
	const netKey = lib.NetworkProviderType
	switch c.opts.NetworkEnv {
	case NetworkEnvVDS:
		if err := os.Setenv(netKey, lib.NetworkProviderTypeVDS); err != nil {
			return err
		}
	case NetworkEnvNSXT:
		if err := os.Setenv(netKey, lib.NetworkProviderTypeNSXT); err != nil {
			return err
		}
	case NetworkEnvNamed:
		if err := os.Setenv(netKey, lib.NetworkProviderTypeNamed); err != nil {
			return err
		}
	default:
		if err := os.Unsetenv(netKey); err != nil {
			return err
		}
	}

	// Configure the feature states.
	for k, v := range c.fssMap {
		if v {
			if err := os.Setenv(k, lib.TrueString); err != nil {
				return err
			}
		} else {
			if err := os.Setenv(k, lib.FalseString); err != nil {
				return err
			}
		}
	}

	if v := c.opts.WithJSONExtraConfig; v != "" {
		if err := os.Setenv("JSON_EXTRA_CONFIG", v); err != nil {
			return err
		}
	} else {
		if err := os.Unsetenv("JSON_EXTRA_CONFIG"); err != nil {
			return err
		}
	}

	return nil
}

func (c *vsphereConfig) initVCSimContentLibrary(ctx context.Context) error {

	librarian := library.NewManager(c.restClient)

	libSpec := library.Library{
		Name: "vmop-content-library",
		Type: "LOCAL",
		Storage: []library.StorageBackings{
			{
				DatastoreID: c.datastore.Reference().Value,
				Type:        "DATASTORE",
			},
		},
	}

	libraryID, err := librarian.CreateLibrary(ctx, libSpec)
	if err != nil {
		return err
	}
	c.opts.ContentLibraryID = libraryID

	createLibItem := func(name string, addrOfResult *string) error {
		libItemSpec := library.Item{
			Name:      name,
			Type:      "ovf",
			LibraryID: c.opts.ContentLibraryID,
		}
		c.opts.ContentLibraryItemIDForLinuxImage = libItemSpec.Name

		libItemID, err := createContentLibraryItemWithLocalFile(
			ctx,
			librarian,
			libItemSpec,
			path.Join(
				testutil.GetRootDirOrDie(),
				"images", "ttylinux-pc_i486-16.1.ovf",
			))
		if err != nil {
			return err
		}
		c.opts.ContentLibraryItemIDForLinuxImage = libItemID

		return nil
	}

	if err := createLibItem(
		"linux",
		&c.opts.ContentLibraryItemIDForLinuxImage); err != nil {

		return nil
	}
	if err := createLibItem(
		"windows",
		&c.opts.ContentLibraryItemIDForWindowsImage); err != nil {

		return nil
	}

	return nil
}

func createContentLibraryItemWithLocalFile(
	ctx context.Context,
	librarian *library.Manager,
	libraryItem library.Item,
	localFilePath string) (string, error) {

	itemID, err := librarian.CreateLibraryItem(ctx, libraryItem)
	if err != nil {
		return "", err
	}

	sessionID, err := librarian.CreateLibraryItemUpdateSession(
		ctx, library.Session{LibraryItemID: itemID})
	if err != nil {
		return "", err
	}

	uploadFunc := func(path string) error {
		f, err := os.Open(filepath.Clean(path))
		if err != nil {
			return err
		}
		defer func() {
			_ = f.Close()
		}()

		fi, err := f.Stat()
		if err != nil {
			return err
		}

		info := library.UpdateFile{
			Name:       filepath.Base(path),
			SourceType: "PUSH",
			Size:       fi.Size(),
		}

		update, err := librarian.AddLibraryItemFile(ctx, sessionID, info)
		if err != nil {
			return err
		}

		u, err := url.Parse(update.UploadEndpoint.URI)
		if err != nil {
			return err
		}

		p := soap.DefaultUpload
		p.ContentLength = info.Size

		return librarian.Client.Upload(ctx, f, u, &p)
	}

	if err := uploadFunc(localFilePath); err != nil {
		return "", err
	}

	if err := librarian.CompleteLibraryItemUpdateSession(
		ctx, sessionID); err != nil {
		return "", err
	}

	return itemID, nil
}

func (c *vsphereConfig) initImageRegistry(
	ctx context.Context,
	klient ctrlclient.Client) error {

	librarian := library.NewManager(c.restClient)

	createImageFn := func(libItemID, imageName, osType, version string) error {
		libItem, err := librarian.GetLibraryItem(ctx, libItemID)
		if err != nil {
			return err
		}

		if c.fssMap[lib.VMServiceV1Alpha2FSS] {
			vmImage := v1alpha2.ClusterVirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name: imageName,
				},
				Spec: v1alpha2.VirtualMachineImageSpec{
					ProviderRef: common.LocalObjectRef{
						Kind: "ClusterContentLibraryItem",
					},
				},
			}
			if err := klient.Create(ctx, &vmImage); err != nil {
				return err
			}
			vmImage.Status = v1alpha2.VirtualMachineImageStatus{
				Name: libItem.Name,
				ProductInfo: v1alpha2.VirtualMachineImageProductInfo{
					FullVersion: version,
				},
				OSInfo: v1alpha2.VirtualMachineImageOSInfo{
					Type: osType,
				},
				ProviderItemID: libItemID,
				Conditions: []metav1.Condition{
					{
						Type:               v1alpha2.VirtualMachineConditionImageReady,
						Status:             metav1.ConditionTrue,
						Reason:             string(metav1.ConditionTrue),
						LastTransitionTime: metav1.Now(),
					},
					{
						Type:               v1alpha2.ReadyConditionType,
						Status:             metav1.ConditionTrue,
						Reason:             string(metav1.ConditionTrue),
						LastTransitionTime: metav1.Now(),
					},
				},
			}
			if err := klient.Status().Update(ctx, &vmImage); err != nil {
				return err
			}
		} else {
			contentSource := v1alpha1.ContentSource{
				ObjectMeta: metav1.ObjectMeta{
					Name: c.opts.ContentLibraryID,
				},
				Spec: v1alpha1.ContentSourceSpec{
					ProviderRef: v1alpha1.ContentProviderReference{
						Name: c.opts.ContentLibraryID,
						Kind: "ContentLibraryProvider",
					},
				},
			}
			if err := klient.Create(ctx, &contentSource); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return err
				}
			}

			contentLibraryProvider := v1alpha1.ContentLibraryProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name: c.opts.ContentLibraryID,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "vmoperator.vmware.com/v1alpha1",
							Kind:       "ContentSource",
							Name:       c.opts.ContentLibraryID,
							UID:        contentSource.UID,
						},
					},
				},
				Spec: v1alpha1.ContentLibraryProviderSpec{
					UUID: c.opts.ContentLibraryID,
				},
			}
			if err := klient.Create(ctx, &contentLibraryProvider); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return err
				}
			}

			vmImage := v1alpha1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name: libItem.Name,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "vmoperator.vmware.com/v1alpha1",
							Kind:       "ContentLibraryProvider",
							Name:       c.opts.ContentLibraryID,
							UID:        contentLibraryProvider.UID,
						},
					},
				},
				Spec: v1alpha1.VirtualMachineImageSpec{
					ProductInfo: v1alpha1.VirtualMachineImageProductInfo{
						FullVersion: version,
					},
					OSInfo: v1alpha1.VirtualMachineImageOSInfo{
						Type: osType,
					},
				},
				Status: v1alpha1.VirtualMachineImageStatus{
					ImageName: libItem.Name,
				},
			}

			if err := klient.Create(ctx, &vmImage); err != nil {
				return err
			}

			vmImage.Status = v1alpha1.VirtualMachineImageStatus{
				ImageName: libItem.Name,
			}
			if err := klient.Status().Update(ctx, &vmImage); err != nil {
				return err
			}
		}

		return nil
	}

	if libItemID := c.opts.ContentLibraryItemIDForLinuxImage; libItemID != "" {
		if err := createImageFn(
			libItemID,
			"vmi-0123456789",
			"otherlinux64guest",
			"v1.0.0"); err != nil {

			return err
		}
	}

	if libItemID := c.opts.ContentLibraryItemIDForWindowsImage; libItemID != "" {
		if err := createImageFn(
			libItemID,
			"vmi-abcdefghij",
			"windows11_64Guest",
			"v1.0.0"); err != nil {

			return err
		}
	}

	return nil
}

func (c *vsphereConfig) initDefaultVMClasses(
	ctx context.Context,
	klient ctrlclient.Client) error {

	vmClass := v1alpha1.VirtualMachineClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "best-effort-small",
		},
		Spec: v1alpha1.VirtualMachineClassSpec{
			Hardware: v1alpha1.VirtualMachineClassHardware{
				Cpus:   int64(2),
				Memory: resource.MustParse("4Gi"),
			},
			Policies: v1alpha1.VirtualMachineClassPolicies{
				Resources: v1alpha1.VirtualMachineClassResources{
					Requests: v1alpha1.VirtualMachineResourceSpec{
						Cpu:    resource.MustParse("1Gi"),
						Memory: resource.MustParse("2Gi"),
					},
					Limits: v1alpha1.VirtualMachineResourceSpec{
						Cpu:    resource.MustParse("2Gi"),
						Memory: resource.MustParse("4Gi"),
					},
				},
			},
		},
	}
	if err := klient.Create(ctx, &vmClass); err != nil {
		return err
	}

	return nil
}
