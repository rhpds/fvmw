# FVMW - Fake VMware API for OpenShift MTV Demos

FVMW is a lightweight mock vCenter API server that allows stock [OpenShift MTV](https://docs.redhat.com/en/documentation/migration_toolkit_for_virtualization/) (Migration Toolkit for Virtualization) to perform end-to-end VM migration demos without a real VMware environment.

Built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator) (Apache 2.0), it presents a fully functional vSphere SOAP API with configurable inventory and disk export support.

## What It Does

- Simulates a vCenter with configurable datacenter, cluster, host, datastore, and VMs
- Implements `ExportVm` + `HttpNfcLease` for VM disk transfer (the critical path for MTV migration)
- Serves pre-built VMDK disk images over HTTP via the lease mechanism
- Runs as a lightweight container (~64MB) on OpenShift
- Multiple users share the same VMDK images via a ReadOnlyMany PVC

## Demo Scenario

Each fvmw instance presents 4 VMs for migration:

| VM | OS | Role | Disk |
|----|----|------|------|
| `database-rhel` | RHEL 9 | PostgreSQL | `db.vmdk` |
| `webserver-1` | Windows 2019 | Web server | `web1.vmdk` |
| `webserver-2` | Windows 2019 | Web server | `web2.vmdk` |
| `haproxy-rhel` | RHEL 9 | HAProxy LB | `haproxy.vmdk` |

## Quick Start

### Local Development

```bash
# Build
make build

# Create stub disks for testing
mkdir -p /tmp/fvmw-disks
touch /tmp/fvmw-disks/{db,web1,web2,haproxy}.vmdk

# Run
FVMW_DISK_PATH=/tmp/fvmw-disks ./bin/fvmw

# Verify with govc
export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:8080/sdk"
export GOVC_INSECURE=1
govc about
govc ls /FVMW-DC/vm/
govc vm.info /FVMW-DC/vm/database-rhel
```

### Deploy to OpenShift

```bash
# Copy and configure local secrets
cp local.env.example local.env
vi local.env  # Set your cluster domain, namespace, image, etc.

# Deploy with Ansible
cd deploy/ansible
ansible-playbook deploy.yml -e @../../local.env
```

See [deploy/ansible/README.md](deploy/ansible/README.md) for full deployment details.

## Architecture

```
Per-user Pod (fvmw):
┌──────────────────────────────────┐
│  fvmw container (~64MB)          │
│                                  │
│  Go binary wrapping vcsim:       │
│  - SOAP API on :8080 (HTTP)      │
│  - Inventory from config.yaml    │
│  - ExportVm -> HttpNfcLease      │
│  - Serves VMDKs via /disk/       │
│                                  │
│  Mounts: /disks (RO shared PVC)  │
└──────────────────────────────────┘

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

### Inventory Config (`config/default.yaml`)

```yaml
datacenter: "FVMW-DC"
cluster: "FVMW-Cluster"
host: "fvmw-esxi.example.com"
datastore: "FVMW-DS"
network: "VM Network"
vms:
  - name: "database-rhel"
    guestId: "rhel9_64Guest"
    disk: "db.vmdk"
    memoryMB: 4096
    numCPUs: 2
```

## Building the Container Image

```bash
make image          # Build with podman
make push           # Push to registry
```

## How MTV Integration Works

1. Create an MTV **VMware Provider** pointing to `https://vcenter-<user>.apps.cluster.example.com/sdk`
2. MTV connects and syncs inventory via the vSphere SOAP API (handled by vcsim)
3. Provider shows 4 VMs with correct guest OS types
4. Create NetworkMapping + StorageMapping
5. Create + execute Migration Plan
6. MTV calls `ExportVm` -> gets `HttpNfcLease` with disk download URLs
7. virt-v2v downloads VMDKs via the lease URLs (served by fvmw from shared PVC)
8. VMs are created in OpenShift Virtualization

## License

Apache 2.0 (govmomi/vcsim dependency is also Apache 2.0)
