# FVMW - Fake VMware API

## Project Overview

FVMW is a lightweight mock vCenter/ESXi API server built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator). It allows stock (unmodified) OpenShift MTV (Migration Toolkit for Virtualization) to perform end-to-end VM migrations in demo environments.

The demo scenario migrates 4 VMs (1 PostgreSQL DB on RHEL, 2 web servers on Windows, 1 HAProxy on RHEL) from "VMware" to OpenShift Virtualization. Multiple users each get their own fvmw instance, sharing the same pre-built VMDK disk images.

## Architecture

- **vcsim wrapper** — Go binary wrapping govmomi's vcsim simulator, adding `ExportVm` + `HttpNfcLease` support
- **Plain HTTP** — fvmw listens on `:8080` (HTTP only); TLS is handled by OpenShift Route (edge termination)
- **Shared disks** — VMDK images on a ReadOnlyMany PVC mounted at `/disks`
- **Per-user isolation** — each user gets a Pod + Service + Route

## Key Technical Decisions

### ExportVm Implementation
vcsim does not implement `ExportVm`. We add it by wrapping `*simulator.VirtualMachine` with `FVMWVirtualMachine` (in `pkg/nfc/export.go`), which embeds the original and adds the `ExportVm` method. The wrapped VMs are registered in the vcsim registry via `ctx.Map.Put()` after inventory creation.

**Why wrapping instead of Map.Handler:** The `Map.Handler` hook gets overridden by `session.Get()` in the simulator's `call()` function (line 214 of `simulator.go`). The session falls through to `s.Map.Get(ref)` which returns the original object. Embedding and replacing in the registry is the only reliable way to add methods.

### HttpNfcLease
The `lease` type embeds `mo.HttpNfcLease` and directly implements `HttpNfcLeaseComplete`, `HttpNfcLeaseAbort`, `HttpNfcLeaseProgress`, and `HttpNfcLeaseGetManifest`. The lease is registered in the session registry via `ctx.Session.Put()`.

### Disk Serving
VMDK files are served on `/disk/<leaseID>/<filename>` (not `/nfc/` to avoid conflict with vcsim's built-in `ServeNFC` handler). The `FVMW_EXTERNAL_HOST` env var controls the hostname in lease device URLs.

### VPX Model
Uses `vpx.ServiceContent` and `vpx.RootFolder` (not ESX). The ESX model doesn't support datacenter creation.

## Project Structure

```
cmd/fvmw/main.go              # Entry point
pkg/inventory/config.go        # YAML config + env var overrides
pkg/inventory/inventory.go     # Builds vcsim model from config
pkg/nfc/export.go              # ExportVm, HttpNfcLease, disk serving
config/default.yaml            # Default inventory (4 VMs)
build/Containerfile            # Multi-stage container build
deploy/                        # Ansible-managed OCP deployment
  ansible/                     # Ansible playbook for OCP install/update
local.env                      # Local secrets (gitignored)
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FVMW_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files |
| `FVMW_EXTERNAL_HOST` | (none) | Hostname for disk download URLs (set to Route hostname) |
| `FVMW_USERNAME` | `administrator@vsphere.local` | vSphere login username |
| `FVMW_PASSWORD` | `password` | vSphere login password |

## Development

```bash
# Build
make build

# Run locally (create stub disk files first)
mkdir -p /tmp/fvmw-disks && touch /tmp/fvmw-disks/{db,web1,web2,haproxy}.vmdk
make run

# Test with govc
export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:8080/sdk"
export GOVC_INSECURE=1
govc ls /FVMW-DC/vm/
govc vm.info /FVMW-DC/vm/database-rhel
```

## Deployment

```bash
# Copy and edit local secrets
cp local.env.example local.env
vi local.env

# Deploy to OCP
cd deploy/ansible
ansible-playbook deploy.yml -e @../../local.env
```

## Build Commands

- `make build` — compile Go binary
- `make image` — build container image
- `make push` — push container image
- `make run` — run locally
- `go vet ./...` — lint
