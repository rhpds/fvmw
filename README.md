# FVMW - Fake VMware API for OpenShift MTV Demos

FVMW is a lightweight mock vCenter API server that allows stock [OpenShift MTV](https://docs.redhat.com/en/documentation/migration_toolkit_for_virtualization/) (Migration Toolkit for Virtualization) to perform end-to-end VM migration demos without a real VMware environment.

Built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator) (Apache 2.0), it presents a fully functional vSphere SOAP API with configurable inventory and disk export support. The default inventory matches the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) demo so migration instructions work unchanged.

## What It Does

- Simulates a vCenter with datacenter, cluster, host, datastore, network, folder, and VMs
- Implements `ExportVm` + `HttpNfcLease` for VM disk transfer (the critical path for MTV migration)
- Serves pre-built VMDK disk images over HTTP via the lease mechanism
- Runs as a lightweight container on OpenShift
- Multiple users share the same VMDK images via a CephFS PVC
- Per-user VM names (e.g. `database-user1`) via `FVMW_USER_SUFFIX` env var

## Demo Scenario

Each fvmw instance presents 4 VMs in `/RS00/vm/Roadshow/` matching the roadshow demo:

| VM | Guest OS | Memory | CPUs | Disk |
|----|----------|--------|------|------|
| `database-{user}` | CentOS 8 (64-bit) | 2048MB | 1 | `database.vmdk` |
| `winweb01-{user}` | Windows Server 2022 (64-bit) | 6144MB | 2 | `winweb01.vmdk` |
| `winweb02-{user}` | Windows Server 2022 (64-bit) | 6144MB | 2 | `winweb02.vmdk` |
| `haproxy-{user}` | Other 2.6.x Linux (64-bit) | 4096MB | 2 | `haproxy.vmdk` |

Infrastructure: Datacenter `RS00`, Cluster `vcs-rs-00`, Network `segment-migrating-to-ocpvirt`

## Quick Start

### Local Development

```bash
# Build
make build

# Create stub disks for testing
mkdir -p /tmp/fvmw-disks
touch /tmp/fvmw-disks/{database,winweb01,winweb02,haproxy}.vmdk

# Run
FVMW_DISK_PATH=/tmp/fvmw-disks FVMW_USER_SUFFIX=-user1 ./bin/fvmw

# Verify with govc
export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:8080/sdk"
export GOVC_INSECURE=1
govc about
govc ls /RS00/vm/Roadshow/
govc vm.info /RS00/vm/Roadshow/database-user1
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

# 4. Deploy per-user instances
ansible-playbook deploy.yml -e @../../local.env
```

See [deploy/ansible/README.md](deploy/ansible/README.md) for full details.

## Architecture

```
Per-user Pod (fvmw):
+----------------------------------+
|  fvmw container (ubi9-minimal)   |
|                                  |
|  Go binary wrapping vcsim:       |
|  - SOAP API on :8080 (HTTP)      |
|  - Inventory from config.yaml    |
|  - ExportVm -> HttpNfcLease      |
|  - Serves VMDKs via /disk/       |
|                                  |
|  Mounts: /disks (shared CephFS)  |
+----------------------------------+

OCP Route (TLS termination):
  https://vcenter-<user>.apps.cluster.example.com
    -> edge TLS -> Service:8080

MTV Provider:
  URL: https://vcenter-<user>.apps.cluster.example.com/sdk
  Credentials: administrator@vsphere.local / <password>
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FVMW_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files |
| `FVMW_EXTERNAL_HOST` | _(none)_ | Route hostname for disk URLs |
| `FVMW_USERNAME` | `administrator@vsphere.local` | Login username |
| `FVMW_PASSWORD` | `password` | Login password |
| `FVMW_USER_SUFFIX` | _(none)_ | Appended to VM names (e.g. `-user1`) |

### Inventory Config (`config/default.yaml`)

```yaml
datacenter: "RS00"
cluster: "vcs-rs-00"
host: "fvmw-esxi-01.example.com"
datastore: "workload_share"
network: "segment-migrating-to-ocpvirt"
folder: "Roadshow"
vms:
  - name: "database"
    guestId: "centos8_64Guest"
    disk: "database.vmdk"
    memoryMB: 2048
    numCPUs: 1
```

## Building the Container Image

```bash
make image          # Build with podman
make push           # Push to registry
```

The OCP BuildConfig (created by `build.yml`) builds automatically from the GitHub repo using the `build/Containerfile` (golang:1.24 builder -> ubi9-minimal runtime).

## How MTV Integration Works

1. Create an MTV **VMware Provider** pointing to `https://vcenter-<user>.apps.<cluster>/sdk`
2. MTV connects and syncs inventory via the vSphere SOAP API (handled by vcsim)
3. Provider shows 4 VMs in `RS00/Roadshow/` with correct guest OS types
4. Create NetworkMapping + StorageMapping
5. Create + execute Migration Plan (select `database-{user}`, `winweb01-{user}`, `winweb02-{user}`)
6. MTV calls `ExportVm` -> gets `HttpNfcLease` with disk download URLs
7. virt-v2v downloads VMDKs via the lease URLs (served by fvmw from shared PVC)
8. VMs are created in OpenShift Virtualization

## License

Apache 2.0 (govmomi/vcsim dependency is also Apache 2.0)
