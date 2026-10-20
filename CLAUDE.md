# FVMW - Fake VMware API

## Project Overview

FVMW is a lightweight mock vCenter/ESXi API server built on [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/simulator). It allows stock (unmodified) OpenShift MTV (Migration Toolkit for Virtualization) to perform end-to-end VM migrations in demo environments without real VMware infrastructure.

The inventory matches the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) demo environment so the migration instructions work unchanged. Each user gets their own fvmw instance with per-user VM names (e.g. `database-user1`, `winweb01-user1`), sharing the same pre-built VMDK disk images via a CephFS PVC.

## Architecture

- **Single shared endpoint** — one fvmw pod/route serves all users at `vcenter.<domain>`
- **Plain HTTP** — fvmw listens on `:8080` (HTTP only); TLS is handled by OpenShift Route (edge termination)
- **XML rewriter** — fixes vcsim XML for libvirt ESX driver compatibility (namespace prefixes, ArrayOf types, empty changeSets)
- **Shared disks** — VMDK images in both flat and streamOptimized format on a ReadWriteMany CephFS PVC
- **Temp datastore** — vcsim VM metadata (.vmx files) written to ephemeral temp dir with symlinks to flat VMDKs
- **VMX generation** — generates minimal VMX files for libvirt to parse via `/folder/` endpoint

## Key Technical Decisions

### Shared Endpoint + Per-User Auth
A single endpoint hosts all users. `FVMW_NUM_USERS` controls generated usernames (`user1..userN`) and each user authenticates with the shared `FVMW_PASSWORD`. The inventory includes per-user VM names (for example `database-user1`, `database-user2`) on one shared vCenter-compatible API endpoint.

### ExportVm Implementation
vcsim does not implement `ExportVm`. We add it by wrapping `*simulator.VirtualMachine` with `FVMWVirtualMachine` (in `pkg/nfc/export.go`), which embeds the original and adds the `ExportVm` method. The wrapped VMs are registered in the vcsim registry via `ctx.Map.Put()` after inventory creation.

**Why wrapping instead of Map.Handler:** The `Map.Handler` hook gets overridden by `session.Get()` in the simulator's `call()` function (line 214 of `simulator.go`). The session falls through to `s.Map.Get(ref)` which returns the original object.

### XML Response Rewriter (`pkg/fixup/xmlrewrite.go`)
vcsim's XML responses are incompatible with libvirt's ESX driver in several ways:
1. **Namespace prefix:** `_XMLSchema-instance:` instead of `xsi:` — rewritten
2. **Empty changeSets:** nil Go values produce `<changeSet>` with no `<val>` — stripped
3. **Untyped `<value>`:** OptionValue values without `xsi:type` — auto-detected
4. **ArrayOf* children:** ALL children of `ArrayOf<Type>` elements need `xsi:type="<Type>"` — general solution scans for ArrayOf declarations and injects types dynamically
5. **HostAgent compatibility (optional):** `apiType` can be rewritten to `HostAgent` when the request host matches `FVMW_ESXI_PREFIX`
6. **Binary paths:** `/folder/` and `/disk/` responses skip the rewriter to prevent OOM on large file downloads

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
  deploy.yml                   # Shared endpoint + direct LoadBalancer listener + per-user auth
  deploy-ui.yml                # Deploy fvmw and verify UI route
  disk-server.yml              # Host flat VMDKs for workshop clusters to download
  prepare-disks.yml            # Populate the shared PVC with VM disks (http/s3/vcenter)
  setup-webhook.yml            # Register GitHub webhook via API
  tasks/prepare-disks.yml      # Shared disk-download task logic
local.env                      # Local secrets (gitignored)
local.env.example              # Template for local.env
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FVMW_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `FVMW_DISK_PATH` | `/disks` | Path to VMDK files |
| `FVMW_EXTERNAL_HOST` | _(none)_ | Hostname for disk download URLs (Route hostname) |
| `FVMW_HOST` | _(none)_ | Hostname shown in inventory (set to shared route hostname) |
| `FVMW_NUM_USERS` | `60` | Number of generated users (`user1..userN`) |
| `FVMW_PASSWORD` | `password` | Shared password used by generated users |
| `FVMW_USERNAME` | `administrator@vsphere.local` | Legacy single-user login username |
| `FVMW_USER_SUFFIX` | _(none)_ | Appended to VM names (e.g. `-user1`) |
| `FVMW_ESX_MODE` | _(none)_ | Legacy ESXi-only mode |
| `FVMW_ESXI_PREFIX` | _(none)_ | Optional host prefix that forces `apiType=HostAgent` |
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

## Operational Memory — 2026-10-20

### Live shared deployment ownership

- On ocpv01, the shared endpoint is `vcenter.apps.ocpv01.rhdp.net`, in namespace `fvmw-infra`, with six replicas and a 200Gi RWX CephFS PVC named `fvmw`.
- Argo application `openshift-gitops/rhpds-infra-fvmw` manages it from `rhpds/infra`, path `helm/fvmw`, branch `main`. This repository's chart is not the source for that deployment. Observed Argo revision: `675966fc8719529e6ea75026778381c671078d71`.
- Local `local.env` and `~/secrets/fvmw-mgmt.kubeconfig` pointed to ocpv-infra01, not ocpv01. Verify the target before operating; use explicit `--kubeconfig` flags. With `oc exec`, put these flags before `--`.
- Argo reported Healthy even with unreadable disks: readiness/liveness check `/about`, not disk access. The observed Route OutOfSync was separate from the disk failure; Deployment and PVC were Synced.

### Migration 403 diagnosis and repair

- Reported failure: virt-v2v HEAD of `/folder/database-user3/database-flat.vmdk?dcPath=RS00&dsName=workloadshare` returned HTTP 403. The later temporary-file unlink error was secondary.
- Reproduced HTTP 403 for all four VM disk URLs, and `Permission denied` reading the database extent on all six replicas.
- Namespace/pod SELinux MCS was `s0:c44,c9`; `/disks` and its eight VMDK files had `container_file_t:s0:c637,c855`. Unix permissions permitted access. Existing privileged maintenance pod `hive-repair` could inspect the volume while application pods could not.
- With explicit repair authorization, used the existing maintenance pod to change only the SELinux level on `/disks` and the eight named files to `s0:c9,c44` (equivalent category ordering). Did not change disk bytes, ownership, Unix permissions, credentials, manifests, or restart pods.
- CephFS clients briefly retained old labels: the first post-repair test passed on only three replicas. Subsequent checks showed corrected labels and successful reads everywhere without further mutation. Do not declare success based only on `chcon` returning zero or one node's view.
- Final verification: all four disks readable on every replica; authenticated public HEAD requests returned 200 with full disk sizes, and GET with `Range: bytes=0-511` returned 206 and 512 bytes for each disk.
- Actual extents are `database-flat-flat.vmdk`, `winweb01-flat-flat.vmdk`, `winweb02-flat-flat.vmdk`, and `haproxy-flat-flat.vmdk`; their corresponding `*-flat.vmdk` PVC files are 512-byte descriptors. The datastore URL's `*-flat.vmdk` symlink correctly resolves to the actual extent.
- What originally changed the labels remains unknown. No evidence established an application-code or chart defect causing the relabeling. Argo reconciliation of the current manifests does not itself repair existing file labels. Never hardcode ocpv01's MCS categories for another namespace or cluster; re-read its current allocation and inspect all volume consumers first.
- Full MTV conversion was not rerun: the affected lab had already been destroyed. Disk-access recovery is verified; end-to-end migration is not.

### Catalog credentials and historical lab

- Provision DB identified GUID `84hv5` as `openshift-cnv.ocp-virt-roadshow-2026.prod`, sandbox `sandbox-84hv5-ocp4-cluster` on ocpv01, with state `destroy-complete` on October 20. The workshop URL suffix `6lkcd` was not the provision GUID.
- FVMW credentials are defined directly in AgnosticV `openshift_cnv/ocp-virt-roadshow-2026/common.yaml`, variable `ocp4_workload_virt_roadshow_fvmw_vcenter_user_password`, as an Ansible Vault block, not a dedicated FVMW credentials include. Production deployer ref observed: `cnv-virt-roadshow-multi-3.0.5`.
- Latest fetched Git vault block matched the live production AgnosticVComponent. Decryption with the existing local vault identity confirmed the password matched ocpv01's `fvmw-password` Secret; inventory authentication as `user3` returned HTTP 200. No credential update was needed.
- An initial comparison incorrectly treated the YAML vault marker as a literal password. That mismatch finding was explicitly retracted. Compare decrypted values in memory and print only equality/status; never print or commit credentials. The deleted lab's original rendered credentials were not recovered.
- Homebrew Ansible failed under Python 3.15 (`dataclasses._is_type` missing); the installed Vault API worked with Python 3.14 for this check. This was a local tooling issue, not failed vault authentication.

### Fleet audit after repair

Read-only snapshot on October 20; reverify before relying on it for a later incident:

| Clusters | Result |
|---|---|
| ocpv01, ocpv02, ocpv03, ocpv06, ocpv07, ocpv08, ocpv09 | Six serving replicas each; all four disks readable and SELinux labels matched the namespace |
| ocpvdev01 | Two serving replicas; same checks passed |
| ocpv-infra01, ocpv-infra02 | No FVMW deployment found |
| ocpv05, ocpv10 | API DNS failed; unverified, not proof of absence or health |

- All eight reachable FVMW endpoints passed authenticated HEAD (200) and 512-byte range GET (206) checks for all four disks; 44 serving replicas passed direct reads. No additional repairs were made.
- ocpv03 access was found under `~/secrets/troshka-shared-ocpv/`; top-level kubeconfig discovery alone missed it. Completed disk-preparation pods are not serving replicas and cannot be exec-tested.
