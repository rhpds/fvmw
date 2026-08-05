# FVMW - Fake VMware API for OpenShift MTV Demos

FVMW is a lightweight mock vCenter/ESXi API server that allows stock [OpenShift MTV](https://docs.redhat.com/en/documentation/migration_toolkit_for_virtualization/) (Migration Toolkit for Virtualization) to perform end-to-end VM migration demos without a real VMware environment.

Built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator) (Apache 2.0), it presents a fully functional vSphere SOAP API with configurable inventory, disk export, and VMX file serving. The default inventory matches the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) demo so migration instructions work unchanged.

## What It Does

- Simulates a vCenter-compatible API on a single shared endpoint
- Full vSphere SOAP API with datacenter, cluster, host, datastore, network, folder, and VMs
- Serves flat VMDK disk images via the `/folder/` endpoint for virt-v2v disk transfer
- Generates VMX files for libvirt domain XML parsing
- XML response rewriter fixes vcsim compatibility issues with libvirt's ESX driver
- Multiple users share the same VMDK images via a CephFS PVC
- Per-user VM names (e.g. `database-user1`) via `FVMW_USER_SUFFIX` env var

## Demo Scenario

One shared endpoint serves all users. Each authenticated user gets their own
4 VM names in `/RS00/vm/Roadshow/`:

| VM | Guest OS | Memory | CPUs | Disk | Controller |
|----|----------|--------|------|------|------------|
| `database-{user}` | CentOS 8 (64-bit) | 2048MB | 1 | 5GB | pvscsi |
| `winweb01-{user}` | Windows Server 2022 (64-bit) | 6144MB | 2 | 21GB | lsilogic-sas |
| `winweb02-{user}` | Windows Server 2022 (64-bit) | 6144MB | 2 | 21GB | lsilogic-sas |
| `haproxy-{user}` | Other 2.6.x Linux (64-bit) | 4096MB | 2 | 4GB | pvscsi |

Infrastructure: Datacenter `RS00`, Cluster `vcs-rs-00`, Network `segment-migrating-to-ocpvirt`

## Quick Start

### Local Development

```bash
# Build
make build

# Start locally (VPX or ESX mode)
./scripts/test-local.sh start        # VPX mode
./scripts/test-local.sh start esx    # ESX mode
./scripts/test-local.sh test         # Run govc tests
./scripts/test-local.sh stop

# Open read-only vCenter-like UI
# (login: user1/password, user2/password, etc.)
xdg-open "http://127.0.0.1:18443/ui/"
```

### Deploy to OpenShift

```bash
# 1. Configure local secrets
cp local.env.example local.env
vi local.env

# 2. One-time bootstrap (with cluster-admin)
oc login <cluster>
cd deploy/ansible
ansible-playbook bootstrap.yml -e @../../local.env

# 3. Set up build pipeline (uses SA kubeconfig from bootstrap)
ansible-playbook build.yml -e @../../local.env

# 4. Prepare disk images (one-time, see Disk Preparation below)

# 5. Deploy per-user instances
ansible-playbook deploy.yml -e @../../local.env
```

See [deploy/ansible/README.md](deploy/ansible/README.md) for full details.

### Deploy with Helm

For deploying `fvmw` itself (Deployment/Service/Route/PVC/Secret) to
additional clusters, a Helm chart is available at
[charts/fvmw](charts/fvmw/README.md):

```bash
helm install fvmw charts/fvmw \
  --namespace fvmw --create-namespace \
  --set clusterDomain=apps.mycluster.example.com \
  --set fvmw.password=changeme
```

Disk preparation (steps 3-4 above) is still done separately via
`deploy/ansible/prepare-disks.yml`, or by pointing `disks.existingClaim` at
an already-populated PVC.

## Architecture

```
Shared Endpoint:

vcenter.<cluster-domain> Route (edge TLS)
                 |
            fvmw Pod
     +----------------------+
     | apiType: VirtualCenter
     | DC: RS00
     | Cluster: vcs-rs-00
     | VMs: *-user1, *-user2, ...
     +----------------------+

Shared CephFS PVC (/disks):
  database.vmdk, database-flat.vmdk
  winweb01.vmdk, winweb01-flat.vmdk
  winweb02.vmdk, winweb02-flat.vmdk
  haproxy.vmdk, haproxy-flat.vmdk

MTV Migration Flow:
  1. Provider -> vcenter route -> fvmw (inventory sync)
  2. virt-v2v -> vcenter route -> fvmw (VM discovery + VMX metadata)
  3. virt-v2v -> vcenter route -> fvmw (download flat VMDK via /folder/)
  4. virt-v2v converts disk -> creates KubeVirt VM
```

## Disk Preparation

VMDK images must be on the shared PVC in **monolithicFlat** format. To prepare:

```bash
# 1. Export VMDKs from a real vCenter (streamOptimized format)
govc export.ovf -vm /DC/vm/database /tmp/export/

# 2. Upload to PVC via a helper pod
oc cp /tmp/export/database-disk-0.vmdk fvmw/helper-pod:/disks/database.vmdk

# 3. Convert to flat format inside the cluster
oc exec helper-pod -- qemu-img convert -O vmdk -o subformat=monolithicFlat \
  /disks/database.vmdk /disks/database-flat.vmdk
```

Both the original (streamOptimized) and flat VMDKs remain on the PVC. The `-flat.vmdk` files are served via the `/folder/` endpoint for virt-v2v.

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FVMW_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files |
| `FVMW_EXTERNAL_HOST` | _(none)_ | Route hostname for ExportVm disk URLs |
| `FVMW_HOST` | _(none)_ | Hostname published in inventory (set to shared endpoint) |
| `FVMW_NUM_USERS` | `60` | Number of generated users (`user1..userN`) |
| `FVMW_PASSWORD` | `password` | Shared password used by generated users |
| `FVMW_USERNAME` | `administrator@vsphere.local` | Legacy single-user login username |
| `FVMW_USER_SUFFIX` | _(none)_ | Appended to VM names (e.g. `-user1`) |
| `FVMW_ESX_MODE` | _(none)_ | Legacy ESXi-only mode |
| `FVMW_ESXI_PREFIX` | _(none)_ | Optional hostname prefix that rewrites `apiType` to `HostAgent` |
| `FVMW_TRACE` | _(none)_ | Set to `1` for SOAP request/response logging |

## How MTV Integration Works

1. Create an MTV **VMware Provider** pointing to `https://vcenter.<cluster>/sdk`
2. MTV connects and syncs inventory via the vSphere SOAP API (handled by vcsim)
3. Provider shows 4 VMs in `RS00/Roadshow/` with correct guest OS types
4. Create NetworkMapping (pod networking) + StorageMapping (ceph-rbd)
5. Create + execute Migration Plan (select `database-{user}`, `winweb01-{user}`, `winweb02-{user}`)
6. virt-v2v connects to the shared endpoint and downloads VMX + flat VMDK files
7. virt-v2v converts the disk and creates a KubeVirt VM in OpenShift Virtualization

## License

Apache 2.0 (govmomi/vcsim dependency is also Apache 2.0)
