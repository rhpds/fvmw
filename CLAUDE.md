# FVMW - Fake VMware API

## Project Overview

FVMW is a lightweight mock vCenter/ESXi API server built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator). It allows stock (unmodified) OpenShift MTV (Migration Toolkit for Virtualization) to perform end-to-end VM migrations in demo environments without real VMware infrastructure.

The inventory matches the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) demo environment so the migration instructions work unchanged. Each user gets their own fvmw instance with per-user VM names (e.g. `database-user1`, `winweb01-user1`), sharing the same pre-built VMDK disk images via a CephFS PVC.

## Architecture

- **Two pods per user** — VPX pod (vCenter, `apiType=VirtualCenter`) + ESXi pod (`apiType=HostAgent`)
- **Plain HTTP** — fvmw listens on `:8080` (HTTP only); TLS is handled by OpenShift Route (edge termination)
- **XML rewriter** — fixes vcsim XML for libvirt ESX driver compatibility (namespace prefixes, ArrayOf types, empty changeSets)
- **Shared disks** — VMDK images in both flat and streamOptimized format on a ReadWriteMany CephFS PVC
- **Temp datastore** — vcsim VM metadata (.vmx files) written to ephemeral temp dir with symlinks to flat VMDKs
- **VMX generation** — generates minimal VMX files for libvirt to parse via `/folder/` endpoint

## Key Technical Decisions

### Dual Pod Architecture
libvirt's `vpx://` driver connects to vCenter first (`apiType=VirtualCenter`), then to the ESXi host separately (`apiType=HostAgent`). A single fvmw instance can't serve both roles, so each user gets two pods:
- `vcenter-<user>` — VPX model with datacenter/cluster/folder hierarchy
- `esxi-<user>` — ESX model with flat VM structure

### ExportVm Implementation
vcsim does not implement `ExportVm`. We add it by wrapping `*simulator.VirtualMachine` with `FVMWVirtualMachine` (in `pkg/nfc/export.go`), which embeds the original and adds the `ExportVm` method. The wrapped VMs are registered in the vcsim registry via `ctx.Map.Put()` after inventory creation.

**Why wrapping instead of Map.Handler:** The `Map.Handler` hook gets overridden by `session.Get()` in the simulator's `call()` function (line 214 of `simulator.go`). The session falls through to `s.Map.Get(ref)` which returns the original object.

### XML Response Rewriter (`pkg/fixup/xmlrewrite.go`)
vcsim's XML responses are incompatible with libvirt's ESX driver in several ways:
1. **Namespace prefix:** `_XMLSchema-instance:` instead of `xsi:` — rewritten
2. **Empty changeSets:** nil Go values produce `<changeSet>` with no `<val>` — stripped
3. **Untyped `<value>`:** OptionValue values without `xsi:type` — auto-detected
4. **ArrayOf* children:** ALL children of `ArrayOf<Type>` elements need `xsi:type="<Type>"` — general solution scans for ArrayOf declarations and injects types dynamically
5. **Binary paths:** `/folder/` and `/disk/` responses skip the rewriter to prevent OOM on large file downloads

### Disk Serving
Two mechanisms for disk access:
- **`/folder/` endpoint** — vcsim's built-in datastore file server; serves flat VMDKs via symlinks from the temp datastore dir to the PVC. Used by virt-v2v for the actual disk transfer.
- **`/disk/` endpoint** — custom handler for ExportVm/HttpNfcLease disk downloads.

### Datastore and Disk Format
- vcsim needs a writable temp dir for VM metadata (.vmx, .nvram, .vmdk descriptors)
- Actual disk images are on the shared PVC in **monolithicFlat** format (raw disk blocks)
- Flat VMDKs created by converting streamOptimized exports with `qemu-img convert -O vmdk -o subformat=monolithicFlat`
- Symlinks: `<tempdir>/<vmname>/<disk>-flat.vmdk` → `/disks/<disk>-flat.vmdk`

### VMX File Generation
libvirt's ESX driver downloads `.vmx` files via the `/folder/` endpoint and parses them to build domain XML. fvmw generates minimal VMX files with correct guest OS, CPU, memory, SCSI controller type, disk, and network configuration.

## Project Structure

```
cmd/fvmw/main.go              # Entry point, HTTP rewriter setup
pkg/inventory/config.go        # YAML config + env var overrides
pkg/inventory/inventory.go     # Builds vcsim model (VPX or ESX mode), VMX generation
pkg/nfc/export.go              # ExportVm, HttpNfcLease, disk serving
pkg/fixup/xmlrewrite.go        # XML response rewriter for libvirt compat
config/default.yaml            # Default inventory matching roadshow demo
build/Containerfile            # Multi-stage: golang:1.24 -> ubi9-minimal
scripts/test-local.sh          # Local start/stop/test helper
deploy/ansible/
  bootstrap.yml                # One-time: namespace, SA, RBAC, kubeconfig
  build.yml                    # BuildConfig, ImageStream, GitHub webhook
  deploy.yml                   # Per-user VPX + ESXi pods, services, routes
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
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files |
| `FVMW_EXTERNAL_HOST` | _(none)_ | Hostname for disk download URLs (Route hostname) |
| `FVMW_HOST` | _(none)_ | ESXi hostname in vCenter inventory (set to ESXi route) |
| `FVMW_USERNAME` | `administrator@vsphere.local` | vSphere login username |
| `FVMW_PASSWORD` | `password` | vSphere login password |
| `FVMW_USER_SUFFIX` | _(none)_ | Appended to VM names (e.g. `-user1`) |
| `FVMW_ESX_MODE` | _(none)_ | Set to `1` for ESXi (HostAgent) mode |
| `FVMW_TRACE` | _(none)_ | Set to `1` for SOAP trace logging |

## Inventory (matching roadshow demo)

| Path | Guest OS | Memory | CPUs | Disk | Controller |
|------|----------|--------|------|------|------------|
| `/RS00/vm/Roadshow/database-{user}` | CentOS 8 | 2048MB | 1 | 5GB | pvscsi |
| `/RS00/vm/Roadshow/winweb01-{user}` | Windows Server 2022 | 6144MB | 2 | 21GB | lsilogic-sas |
| `/RS00/vm/Roadshow/winweb02-{user}` | Windows Server 2022 | 6144MB | 2 | 21GB | lsilogic-sas |
| `/RS00/vm/Roadshow/haproxy-{user}` | Linux 2.6.x | 4096MB | 2 | 4GB | pvscsi |

All VMs: EFI firmware, Secure Boot disabled, powered off.

## Development

```bash
# Build
make build

# Run locally
./scripts/test-local.sh start        # VPX mode
./scripts/test-local.sh start esx    # ESX mode
./scripts/test-local.sh test         # Run govc tests
./scripts/test-local.sh stop

# Test with govc
export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:18443/sdk"
export GOVC_INSECURE=1
govc ls /RS00/vm/Roadshow/
```

## Deployment Workflow

```bash
# 1. One-time bootstrap (with cluster-admin)
oc login <cluster>
cd deploy/ansible
ansible-playbook bootstrap.yml -e @../../local.env

# 2. Set up build pipeline (with SA kubeconfig)
ansible-playbook build.yml -e @../../local.env

# 3. Export and convert disk images (one-time)
# See deploy/ansible/README.md for disk preparation steps

# 4. Deploy per-user instances
ansible-playbook deploy.yml -e @../../local.env

# 5. Teardown
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

## Cluster Access

- **SA kubeconfig:** `~/secrets/fvmw-mgmt.kubeconfig`
- **SA:** `system:serviceaccount:fvmw:fvmw-mgmt`
- **Permissions:** namespace, deployments, services, routes (incl custom-host), builds, imagestreams, secrets, PVCs

## Build Commands

- `make build` — compile Go binary
- `make image` — build container image with podman
- `make push` — push container image
- `make run` — run locally
- `make deploy` — run deploy playbook
- `make teardown` — run teardown playbook
- `go vet ./...` — lint
