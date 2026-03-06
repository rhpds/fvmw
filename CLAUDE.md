# FVMW - Fake VMware API

## Project Overview

FVMW is a lightweight mock vCenter/ESXi API server built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator). It allows stock (unmodified) OpenShift MTV (Migration Toolkit for Virtualization) to perform end-to-end VM migrations in demo environments without real VMware infrastructure.

The inventory matches the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) demo environment so the migration instructions work unchanged. Each user gets their own fvmw instance with per-user VM names (e.g. `database-user1`, `winweb01-user1`), sharing the same pre-built VMDK disk images via a CephFS PVC.

## Architecture

- **vcsim wrapper** — Go binary wrapping govmomi's vcsim simulator, adding `ExportVm` + `HttpNfcLease` support
- **Plain HTTP** — fvmw listens on `:8080` (HTTP only); TLS is handled by OpenShift Route (edge termination)
- **Shared disks** — VMDK images on a ReadWriteMany CephFS PVC mounted at `/disks`
- **Temp datastore** — vcsim VM metadata written to an ephemeral temp dir (not the PVC)
- **Per-user isolation** — each user gets a Pod + Service + Route

## Key Technical Decisions

### ExportVm Implementation
vcsim does not implement `ExportVm`. We add it by wrapping `*simulator.VirtualMachine` with `FVMWVirtualMachine` (in `pkg/nfc/export.go`), which embeds the original and adds the `ExportVm` method. The wrapped VMs are registered in the vcsim registry via `ctx.Map.Put()` after inventory creation.

**Why wrapping instead of Map.Handler:** The `Map.Handler` hook gets overridden by `session.Get()` in the simulator's `call()` function (line 214 of `simulator.go`). The session falls through to `s.Map.Get(ref)` which returns the original object. Embedding and replacing in the registry is the only reliable way to add methods.

### HttpNfcLease
The `lease` type embeds `mo.HttpNfcLease` and directly implements `HttpNfcLeaseComplete`, `HttpNfcLeaseAbort`, `HttpNfcLeaseProgress`, and `HttpNfcLeaseGetManifest`. The lease is registered in the session registry via `ctx.Session.Put()`.

### Disk Serving
VMDK files are served on `/disk/<leaseID>/<filename>` (not `/nfc/` to avoid conflict with vcsim's built-in `ServeNFC` handler). The `FVMW_EXTERNAL_HOST` env var controls the hostname in lease device URLs.

### Datastore Separation
vcsim needs a writable directory for VM metadata (.vmx files, directories). The datastore is backed by `os.MkdirTemp()`, while actual VMDK files are read from `FVMW_DISK_PATH` (the shared PVC). Stub VMDK files are created in the temp dir so vcsim's disk-add validation passes.

### VPX Model
Uses `vpx.ServiceContent` and `vpx.RootFolder` (not ESX). The ESX model doesn't support datacenter creation.

## Project Structure

```
cmd/fvmw/main.go              # Entry point
pkg/inventory/config.go        # YAML config + env var overrides
pkg/inventory/inventory.go     # Builds vcsim model from config
pkg/nfc/export.go              # ExportVm, HttpNfcLease, disk serving
config/default.yaml            # Default inventory matching roadshow demo
build/Containerfile            # Multi-stage: golang:1.24 -> ubi9-minimal
deploy/ansible/
  bootstrap.yml                # One-time: namespace, SA, RBAC, kubeconfig
  build.yml                    # BuildConfig, ImageStream, GitHub webhook
  deploy.yml                   # Per-user Deployment, Service, Route
  setup-webhook.yml            # Register GitHub webhook via API
  tasks/deploy-user.yml        # Per-user resource creation
  tasks/teardown-user.yml      # Per-user cleanup
local.env                      # Local secrets (gitignored)
local.env.example              # Template for local.env
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FVMW_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files (read-only source) |
| `FVMW_EXTERNAL_HOST` | _(none)_ | Hostname for disk download URLs (Route hostname) |
| `FVMW_USERNAME` | `administrator@vsphere.local` | vSphere login username |
| `FVMW_PASSWORD` | `password` | vSphere login password |
| `FVMW_USER_SUFFIX` | _(none)_ | Appended to VM names (e.g. `-user1`) |

## Inventory (matching roadshow demo)

The default config presents VMs matching the real vCenter used by the roadshow:

| Path | Guest OS | Memory | CPUs | Disk |
|------|----------|--------|------|------|
| `/RS00/vm/Roadshow/database-{user}` | CentOS 8 | 2048MB | 1 | `database.vmdk` |
| `/RS00/vm/Roadshow/winweb01-{user}` | Windows Server 2022 | 6144MB | 2 | `winweb01.vmdk` |
| `/RS00/vm/Roadshow/winweb02-{user}` | Windows Server 2022 | 6144MB | 2 | `winweb02.vmdk` |
| `/RS00/vm/Roadshow/haproxy-{user}` | Linux 2.6.x | 4096MB | 2 | `haproxy.vmdk` |

Datacenter: `RS00`, Cluster: `vcs-rs-00`, Network: `segment-migrating-to-ocpvirt`

## Development

```bash
# Build
make build

# Create stub disks for testing
mkdir -p /tmp/fvmw-disks
touch /tmp/fvmw-disks/{database,winweb01,winweb02,haproxy}.vmdk

# Run locally
FVMW_DISK_PATH=/tmp/fvmw-disks FVMW_USER_SUFFIX=-user1 make run

# Test with govc
export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:8080/sdk"
export GOVC_INSECURE=1
govc ls /RS00/vm/Roadshow/
govc vm.info /RS00/vm/Roadshow/database-user1
```

## Deployment Workflow

```bash
# 1. One-time bootstrap (with cluster-admin)
oc login <cluster>
cd deploy/ansible
ansible-playbook bootstrap.yml -e @../../local.env

# 2. Set up build pipeline (with SA kubeconfig)
ansible-playbook build.yml -e @../../local.env

# 3. Deploy per-user instances
ansible-playbook deploy.yml -e @../../local.env

# 4. Teardown
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

## Cluster Access

- **SA kubeconfig:** `~/secrets/fvmw-mgmt.kubeconfig`
- **SA:** `system:serviceaccount:fvmw:fvmw-mgmt`
- **Permissions:** namespace, deployments, services, routes, builds, imagestreams, secrets, PVCs

## Build Commands

- `make build` — compile Go binary
- `make image` — build container image with podman
- `make push` — push container image
- `make run` — run locally
- `make deploy` — run deploy playbook
- `make teardown` — run teardown playbook
- `go vet ./...` — lint
