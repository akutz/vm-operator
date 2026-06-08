# VM Operator vs. KubeVirt

[KubeVirt](https://kubevirt.io) is a CNCF project that runs virtual machines natively on Kubernetes worker nodes using QEMU/KVM. Both VM Operator and KubeVirt expose VMs as Kubernetes Custom Resources, but they solve fundamentally different problems for different audiences and infrastructure stacks.

!!! abstract "Summary"

    **VM Operator** is a Kubernetes operator purpose-built for VMware vSphere environments. It acts as a translation layer between the Kubernetes API and the vSphere infrastructure stack — VMs are actually provisioned and run by ESXi; VM Operator just orchestrates them declaratively. It is the foundation of VMware Cloud Foundation (VCF) and vSphere with Tanzu workload management.

    **KubeVirt** runs VMs directly on Kubernetes worker nodes using QEMU/KVM. It does not require a separate hypervisor infrastructure — it turns Kubernetes nodes themselves into hypervisors. VMs run as Kubernetes Pods (one Pod per VM) via a multi-component architecture (`virt-controller`, `virt-handler`, `virt-launcher`, `virt-api`).

    Neither project is a drop-in replacement for the other. The hypervisor model mismatch is fundamental. However, a [KubeVirt-compatible API shim on vSphere](#kubevirt-compatible-api-on-vsphere) is a tractable path to API-level portability, following the same pattern as `cluster-api-provider-vsphere`.

## Design Philosophy

The two projects have opposite philosophies about where complexity should live.

**VM Operator** treats Kubernetes as a _control plane for vSphere_. Its design hides vSphere complexity from developers — they see `VirtualMachine`, `VirtualMachineClass`, and `VirtualMachineImage`, never ESXi hosts, resource pools, or datastores. All hardware-level concerns (CPU scheduling, memory, I/O, networking) remain in vSphere. The operator simply translates Kubernetes intent into vSphere API calls.

**KubeVirt** follows the _Kubernetes Razor_ — reuse as much of Kubernetes as possible rather than reinventing it for VMs. Each VM maps to a Pod (inheriting scheduling, networking, storage, and RBAC). VM disk images are distributed as OCI container images. The VM/VMI separation mirrors Deployment/Pod, enabling restart semantics and fleet management. Multiple independent components watch shared Kubernetes state (choreography) rather than a single orchestrator — so QEMU continues running even if `virt-controller` restarts.

## At a Glance

| Dimension | VM Operator | KubeVirt |
|-----------|-------------|----------|
| **Hypervisor** | VMware ESXi (external vSphere infrastructure) | QEMU/KVM (or Hyper-V) on Linux cluster nodes |
| **Compute backend** | External ESXi hosts managed by vCenter | Kubernetes worker nodes |
| **Primary use case** | Unified K8s+VM management on vSphere | Running VMs natively on Kubernetes clusters |
| **API stability** | `v1alpha6` (alpha versioning; VCF/Tanzu GA) | `kubevirt.io/v1` (stable) |
| **VM model** | Single `VirtualMachine` CRD | `VirtualMachine` (durable) + `VirtualMachineInstance` (ephemeral) |
| **Networking** | NSX-T VPC/NCP, NetOp VDS, Named | Multus, SR-IOV, masquerade, passt, bridge |
| **Storage** | vSphere CSI + Content Library | PVCs, DataVolumes (CDI), ContainerDisk |
| **Image distribution** | VMware Content Library (OVA/OVF/VMDK) | OCI container images wrapping qcow2/raw |
| **Guest configuration** | CloudInit, LinuxPrep, Sysprep, VAppConfig | CloudInit NoCloud/ConfigDrive, Ignition, Sysprep |
| **Live migration** | vSphere vMotion | QEMU live migration (libvirt) |
| **Multi-architecture** | x86-64 only (ESXi constraint) | x86-64, aarch64, s390x |
| **Multi-tenancy** | vSphere namespace + Kubernetes namespace | Kubernetes namespace |
| **Governance** | Broadcom / VMware (Apache 2.0) | CNCF incubating (Apache 2.0) |
| **CLI** | `kubectl` only | `virtctl` (purpose-built VM CLI) |

## Architecture

### VM Operator

VM Operator follows the standard `controller-runtime` operator pattern — a single binary hosting 20+ controllers plus webhooks. Its key architectural characteristic is the **external hypervisor model**: the controller communicates out to vSphere via the vSphere API (`govmomi`) rather than running any hypervisor software in-cluster.

```
Kubernetes API Server
        │
        ▼
vmoperator-controller-manager
 ├── VirtualMachine controller ───────────────────────────────┐
 ├── VirtualMachineClass controller                           │  govmomi
 ├── VirtualMachineImage controller                           ├──────────► vCenter / vSphere API
 ├── VirtualMachineService controller                         │
 ├── VirtualMachineImageCache controller                      │
 └── (15+ additional controllers)                            │
                                                              ▼
 Webhooks (validate + defaulting)                        ESXi Hosts
                                                    (run actual VMs)
```

### KubeVirt

KubeVirt uses a **multi-component choreography architecture** where several independently deployed services each watch Kubernetes object state and react to changes. For each `VirtualMachineInstance`, `virt-controller` renders a `virt-launcher` Pod that hosts a local `libvirtd` daemon and a QEMU process.

```
User → kubectl / virtctl
         │
         ▼
   virt-api (webhooks, subresources: console, VNC, migrate, pause)
         │
   Kubernetes API Server (CRDs)
         │
   virt-controller ──────────────────────► virt-launcher Pod (per VMI)
   (VM RunStrategy, VMI→Pod lifecycle)      ├── compute container
         │                                  │    ├── libvirtd
   virt-handler (DaemonSet, per node)       │    └── QEMU/KVM process
         └── gRPC ──────────────────────────┘
              (SyncVMI, MigrateVMI, etc.)
```

## API Design

### VM Operator CRDs

VM Operator groups all resources under the `vmoperator.vmware.com` API group. Its current storage version is `v1alpha6`.

| CRD | Scope | Purpose |
|-----|-------|---------|
| `VirtualMachine` | Namespaced | Core VM object: spec, status, power state |
| `VirtualMachineClass` | Namespaced | Hardware profile (CPU, memory, devices) |
| `VirtualMachineClassBinding` | Namespaced | Grants a namespace access to a class |
| `VirtualMachineClassInstance` | Namespaced | Immutable snapshot of a class at VM creation time |
| `VirtualMachineImage` | Namespaced | VM template image (Content Library item) |
| `ClusterVirtualMachineImage` | Cluster | Cluster-wide VM template |
| `VirtualMachineImageCache` | Namespaced | Pre-staged image cache for fast deploy |
| `VirtualMachineService` | Namespaced | Load-balancer / ClusterIP service for VMs |
| `VirtualMachineSetResourcePolicy` | Namespaced | Maps VMs to vSphere resource pools/folders |
| `VirtualMachineWebConsoleRequest` | Namespaced | Requestable VM console access token |
| `VirtualMachineSnapshot` _(alpha)_ | Namespaced | VM snapshot request |
| `VirtualMachineRestore` _(alpha)_ | Namespaced | VM restore from snapshot |
| `VirtualMachineGroup` _(alpha)_ | Namespaced | Groups VMs for coordinated operations |

### KubeVirt CRDs

KubeVirt spans multiple API groups. Its core API (`kubevirt.io/v1`) is stable.

| CRD | API Group | Scope | Purpose |
|-----|-----------|-------|---------|
| `VirtualMachine` | `kubevirt.io/v1` | Namespaced | Durable VM; owns VMI and DataVolumeTemplates |
| `VirtualMachineInstance` | `kubevirt.io/v1` | Namespaced | Ephemeral running VM; 1:1 with virt-launcher Pod |
| `VirtualMachineInstanceMigration` | `kubevirt.io/v1` | Namespaced | Live migration request |
| `VirtualMachineInstanceReplicaSet` | `kubevirt.io/v1` | Namespaced | N replicas of a VMI template |
| `VirtualMachinePool` | `pool.kubevirt.io/v1alpha1` | Namespaced | Fleet with rolling update support |
| `VirtualMachineInstancetype` | `instancetype.kubevirt.io/v1beta1` | Namespaced | Named CPU/memory instance type |
| `VirtualMachineClusterInstancetype` | `instancetype.kubevirt.io/v1beta1` | Cluster | Cluster-scoped instance type |
| `VirtualMachinePreference` | `instancetype.kubevirt.io/v1beta1` | Namespaced | Named non-resource preferences |
| `VirtualMachineSnapshot` | `snapshot.kubevirt.io/v1beta1` | Namespaced | VM snapshot |
| `VirtualMachineRestore` | `snapshot.kubevirt.io/v1beta1` | Namespaced | Restore from snapshot |
| `VirtualMachineClone` | `clone.kubevirt.io/v1alpha1` | Namespaced | VM clone operation |
| `MigrationPolicy` | `migrations.kubevirt.io/v1alpha1` | Cluster | Per-namespace migration configuration |
| `KubeVirt` | `kubevirt.io/v1` | Cluster | KubeVirt installation configuration |

### Key API differences

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| VM / instance split | No — single `VirtualMachine` object | Yes — `VirtualMachine` (durable) + `VirtualMachineInstance` (ephemeral) |
| Power / restart policy | `PowerState: PoweredOn \| PoweredOff \| Suspended` | `RunStrategy: Always \| Halted \| Manual \| RerunOnFailure \| Once` |
| Hardware exposure | Abstract / class-based (admin-defined profiles) | Full hardware spec: CPU topology, firmware, devices |
| Fleet management | External tooling required | `VirtualMachinePool`, `VMIReplicaSet` |
| Confidential compute | Not present | AMD SEV/SEV-SNP, Intel TDX, IBM Secure Execution |

## VM Lifecycle

### Power state model

=== "VM Operator"

    VM Operator uses a single `spec.powerState` field:

    - `PoweredOn` — the VM should be running.
    - `PoweredOff` — the VM should be off.
    - `Suspended` — the VM should be suspended (vSphere memory checkpoint).

    There is no built-in restart-on-failure policy. External tooling or the `check` annotation mechanism can participate in the lifecycle.

=== "KubeVirt"

    KubeVirt uses `spec.runStrategy` on the `VirtualMachine` object to express restart policy:

    - `Always` — keep a `VirtualMachineInstance` running; restart on any failure.
    - `Halted` — ensure no VMI exists.
    - `Manual` — create/delete VMI only via explicit `StateChangeRequest`.
    - `RerunOnFailure` — restart on failure only (not on clean shutdown).
    - `Once` — create VMI once; never restart after it stops.
    - `WaitAsReceiver` — create a receiver VMI waiting for an incoming live migration.

    The `VirtualMachineInstance` object is ephemeral; it is deleted and re-created on each restart.

### Notable lifecycle features

| Feature | VM Operator | KubeVirt |
|---------|-------------|----------|
| Crash-loop restart policy | Not built-in (`check` annotation) | `RunStrategy: Always / RerunOnFailure` with backoff |
| Fleet scale (N replicas) | External tooling | `VirtualMachinePool`, `VMIReplicaSet` |
| External lifecycle participation | `vmoperator.vmware.com/check` annotation | Not present |
| Prevent deletion | `vmoperator.vmware.com/skip-delete` annotation | Not present |
| Pause (in-memory) | `PowerState: Suspended` (full vSphere suspend) | `virtctl pause` (`virDomainSuspend`, no snapshot) |

## Networking

=== "VM Operator"

    VM Operator's networking is handled entirely within vSphere. Four CNI backends are supported:

    | Backend | Technology | Notes |
    |---------|-----------|-------|
    | `NetOp` / `VDS` | VMware vSphere Distributed Switch | DVPortgroup-based port groups |
    | `NCP` | NSX-T Container Plugin | NSX-T logical segments; micro-segmentation |
    | `VPC` | NSX-T VPC | NSX-T VPC-mode; per-namespace segments |
    | `Named` | Named networks | Pre-existing named port groups |

    IP addresses are allocated by the vSphere network backend (NSX-T IPAM, DHCP, or static) and reported back via VMware guest info. Kubernetes CNI is not involved for VM network interfaces.

    `VirtualMachineService` is a VM Operator CRD that creates a load-balanced endpoint for a set of VMs, backed by NSX-T or kube-proxy.

=== "KubeVirt"

    KubeVirt follows the "KubeVirt Razor" — reuse existing Kubernetes CNI rather than building VM-specific networking.

    **Network sources:**

    - `Pod` — VM uses the Pod's primary CNI network.
    - `Multus` — references a `NetworkAttachmentDefinition` for secondary interfaces.
    - `ResourceClaim` — DRA-based network device allocation (Alpha).

    **Interface binding methods:**

    | Method | Description |
    |--------|-------------|
    | `Masquerade` | NAT via iptables/nftables; in-pod DHCP server. Default for pod network. |
    | `Bridge` | Linux bridge; VM shares the Pod IP directly. |
    | `SRIOV` | PCI passthrough of an SR-IOV Virtual Function. Near line-rate performance. |
    | `Passt` _(Beta)_ | Usermode networking; no root privileges required. |
    | Binding plugins _(Alpha)_ | Extensible framework for custom binding via CNI sidecar containers. |

    VMs are exposed via standard Kubernetes `Service` resources (the VM's Pod IP is reachable from within the cluster).

## Storage

=== "VM Operator"

    All VM Operator storage is vSphere CSI-based. VM volumes are Kubernetes PersistentVolumeClaims backed by the vSphere CSI driver, which maps them to CNS volumes on vSAN or other vSphere datastores.

    | Feature | Notes |
    |---------|-------|
    | VM home directory | Backed by a vSphere datastore; selected via `spec.storageClass` |
    | Additional volumes | PVC-backed (`spec.volumes[].persistentVolumeClaim`) |
    | Disk controllers | SCSI, NVMe, SATA, IDE — configurable per volume |
    | Shared disks | Multi-writer PVCs for Oracle RAC, WSFC |
    | Instance Storage | Local SSD-backed ephemeral volumes for low-latency workloads |
    | Fast Deploy / image caching | `VirtualMachineImageCache` pre-stages OVF files to datastores for linked-clone or direct-copy deploys |
    | Snapshots _(alpha)_ | `VirtualMachineSnapshot` / `VirtualMachineRestore` |

=== "KubeVirt"

    KubeVirt uses standard Kubernetes PVCs and integrates with CDI (Containerized Data Importer) for VM disk image import and management.

    | Volume type | Description |
    |-------------|-------------|
    | `ContainerDisk` | VM disk image as OCI container image; COW overlay at runtime; ephemeral |
    | `PersistentVolumeClaim` | Standard K8s PVC; any CSI driver; persistent |
    | `DataVolume` | CDI-managed PVC; auto-populates from URL, OCI registry, PVC clone, or snapshot |
    | `Ephemeral` | PVC-backed with COW overlay; writes lost on VM stop |
    | `EmptyDisk` | Sparse scratch disk; VMI lifetime |
    | `CloudInitNoCloud` / `CloudInitConfigDrive` | ISO for cloud-init |
    | `ConfigMap` / `Secret` | Kubernetes objects exposed as disk |
    | `VirtioFS` | Shared filesystem from host container path |

    Hotplug (live attach/detach of PVCs) is GA. Storage live migration between storage classes is supported (`updateVolumesStrategy: Migration`).

## VM Images

=== "VM Operator"

    VM images are sourced from **VMware Content Library**, a vCenter feature for storing and distributing VM templates (OVA/OVF), ISO files, and other content.

    - `ClusterVirtualMachineImage` — cluster-wide image, synced from a Content Library subscription.
    - `VirtualMachineImage` — namespace-scoped image.
    - `VirtualMachineImageCache` — pre-stages image files to a vSAN datastore for fast linked-clone or direct-copy deploys.

    Images are referenced by name in `spec.image`. The image status exposes capabilities such as `supportsNetworkCustomization` and `hasSupportedGuestFamily` that gate which bootstrap methods are available.

=== "KubeVirt"

    KubeVirt packages VM disk images as **OCI container images** (called _ContainerDisk_). A disk image (qcow2 or raw) is embedded in an OCI image at a well-known path (`/disk/disk.img`) and distributed through any standard OCI registry (Docker Hub, Quay.io, a private Harbor instance, etc.).

    ```yaml
    volumes:
    - name: rootdisk
      containerDisk:
        image: quay.io/kubevirt/fedora-cloud-container-disk-demo:latest
    ```

    `virt-controller` adds an init container for each ContainerDisk volume that extracts the disk image and serves it over a Unix socket. `virt-launcher` reads the image via this socket and creates a qcow2 COW overlay.

    CDI `DataVolume` objects can also import disk images from HTTP URLs, OCI registries, or by cloning existing PVCs, producing persistent PVC-backed volumes.

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| Image format | OVA / OVF / VMDK | OCI image wrapping qcow2 or raw |
| Image registry | VMware Content Library | OCI registry (Docker Hub, Quay, etc.) |
| Image toolchain | Content Library CLI / vCenter UI | `docker build`, `podman`, `buildah` |
| Copy-on-write | vSphere linked clones (VMDK) | qcow2 overlay on ContainerDisk |
| Image caching | Pre-staged to vSAN datastore | OCI layer cache per node |

## Guest OS Configuration

=== "VM Operator"

    VM Operator supports four bootstrap providers via `spec.bootstrap`:

    | Provider | Target OS | Mechanism |
    |----------|-----------|-----------|
    | `CloudInit` | Linux | Cloud-init via VMware `guestinfo` / OVF env transport |
    | `LinuxPrep` | Linux | VMware Guest OS Customization (GOSC); sets hostname, DNS, NIC config via VMware Tools |
    | `Sysprep` | Windows | Windows Sysprep via GOSC; `unattend.xml` for Mini-Setup |
    | `VAppConfig` | Any | OVF vApp properties injected via `guestinfo.ovfenv`; used by appliances |

    Network configuration (IPs, gateway, DNS) is passed automatically from the network backend allocation into the bootstrap payload.

=== "KubeVirt"

    | Method | Mechanism |
    |--------|-----------|
    | `CloudInitNoCloud` | ISO 9660 CDROM; inline or Secret-referenced `userData` / `networkData` |
    | `CloudInitConfigDrive` | OpenStack Config Drive format ISO |
    | `Sysprep` | `autounattend.xml` in ConfigMap/Secret, attached as CDROM |
    | `Ignition` _(Alpha)_ | Fedora CoreOS / RHCOS; annotation-injected JSON |
    | `AccessCredentials` | SSH key injection into running VMs via QEMU Guest Agent (dynamic rotation) |

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| Cloud-init transport | VMware guestinfo / OVF env | ISO CDROM (NoCloud or Config Drive) |
| VMware-specific | LinuxPrep, VAppConfig | Not applicable |
| SSH key injection (running VM) | Not natively supported | Yes, via QEMU Guest Agent |
| Ignition (CoreOS) | Not supported | Alpha |

## Scheduling and Placement

=== "VM Operator"

    VM Operator delegates scheduling to **vSphere DRS** (Distributed Resource Scheduler). The placement engine in VM Operator queries vSphere for placement recommendations, taking resource pool capacity, host availability, and storage compatibility into account. The Kubernetes scheduler is not involved in VM placement decisions.

    - `VirtualMachineSetResourcePolicy` maps VMs to specific vSphere resource pools and folders.
    - Zone awareness is supported for vSphere multi-zone topologies (vSphere clusters as Kubernetes failure domains).

=== "KubeVirt"

    KubeVirt VMs are scheduled by the **Kubernetes scheduler** as regular Pods (`virt-launcher` Pods). `virt-handler` runs a node labeler on each node to advertise capabilities:

    - `cpu-model.node.kubevirt.io/<model>` — CPU model support.
    - `cpu-feature.node.kubevirt.io/<feature>` — CPU feature flags.
    - `machine-type.node.kubevirt.io/<type>` — supported QEMU machine types.

    VMI specs use standard `nodeSelector`, `affinity`, and `tolerations` fields. `DedicatedCPUPlacement` requests exclusive CPU cores via the Kubernetes CPU manager (static policy).

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| Scheduler | vSphere DRS | Kubernetes scheduler |
| CPU pinning | vSphere CPU affinity | K8s CPU manager (`DedicatedCPUPlacement`) |
| NUMA awareness | vSphere (ESXi-managed) | `CPU.NUMA` + K8s NUMA topology manager |
| GPU / PCIe passthrough | vSphere DirectPath I/O | K8s device plugins + DRA |

## Live Migration

=== "VM Operator"

    VM Operator uses **vSphere vMotion** — VMware's production-grade live migration technology. vMotion migrates a running VM (memory, storage, network state) from one ESXi host to another with near-zero downtime. It is triggered and managed by vSphere (DRS, maintenance mode, or manually from vCenter); VM Operator does not expose vMotion as a Kubernetes API object.

=== "KubeVirt"

    KubeVirt implements live migration using **QEMU's memory migration protocol** over the Kubernetes network. The migration is triggered by creating a `VirtualMachineInstanceMigration` object (or via `virtctl migrate`).

    1. Migration controller creates a target `virt-launcher` Pod on a different node.
    2. `virt-handler` on the source node initiates QEMU iterative memory copy.
    3. At convergence, QEMU pauses the source, transfers final pages, and resumes on the target.
    4. A `PodDisruptionBudget` is created automatically during migration.

    `MigrationPolicy` (cluster-scoped) configures per-namespace migration parameters: bandwidth limits, parallelism, auto-convergence, and post-copy mode.

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| Technology | vSphere vMotion (hardware-level) | QEMU live migration (software) |
| API representation | Not a K8s object (vSphere-managed) | `VirtualMachineInstanceMigration` CRD |
| Trigger | vSphere DRS / maintenance mode / vCenter | `virtctl migrate`, K8s API, or automatic eviction |
| Post-copy mode | Not applicable | Supported (`AllowPostCopy`) |
| Storage migration | Storage vMotion (vSphere) | `updateVolumesStrategy: Migration` |

## Security and Multi-Tenancy

=== "VM Operator"

    - Each namespace corresponds to a vSphere namespace (sub-resource pool) with resource quotas enforced at the hardware level.
    - `VirtualMachineClassBinding` requires cluster-admin approval to grant hardware profile access to a namespace.
    - `VirtualMachineWebConsoleRequest` generates a time-limited, ticket-based console URL backed by vSphere WMKS, providing audited console access without direct vSphere credentials.
    - Network policy is enforced by NSX-T's distributed firewall (stateful, hardware-level).

=== "KubeVirt"

    - `virt-launcher` Pods run as non-root by default with a custom seccomp profile (Beta).
    - SELinux MCS level is configurable per VMI for namespace-level isolation.
    - Confidential computing is supported: AMD SEV, SEV-SNP, Intel TDX (Trust Domain Extensions), and IBM Secure Execution allow VMs with encrypted memory that the hypervisor cannot read.
    - `MigrationPolicy` (cluster-scoped) configures migration behavior per namespace/label selector.

| Aspect | VM Operator | KubeVirt |
|--------|-------------|----------|
| Multi-tenancy unit | vSphere namespace + K8s namespace | Kubernetes namespace |
| Resource isolation | vSphere resource pools (hardware) | K8s resource requests/limits (cgroup) |
| Confidential compute | Not present | SEV, SEV-SNP, TDX, IBM Secure Execution |
| Console access | Requestable ticket (WMKS, audited) | `virtctl console` / `virtctl vnc` |
| Network policy | NSX-T DFW (stateful distributed firewall) | K8s NetworkPolicy + CNI |

