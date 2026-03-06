# FVMW Ansible Deployment

Ansible playbooks to deploy and manage fvmw instances on OpenShift.

## Prerequisites

- `ansible` with `kubernetes.core` collection:
  ```bash
  ansible-galaxy collection install kubernetes.core
  ```
- `oc` CLI (for initial bootstrap only)

## Playbooks

| Playbook | Purpose | Requires |
|----------|---------|----------|
| `workshop-setup.yml` | **Full workshop setup** (build, disks, pods, MTV) | cluster-admin kubeconfig |
| `bootstrap.yml` | One-time namespace, SA, RBAC, kubeconfig setup | cluster-admin |
| `build.yml` | BuildConfig, ImageStream, GitHub webhook | SA kubeconfig |
| `deploy.yml` | Per-user VPX + ESXi pods, services, routes | SA kubeconfig |
| `disk-server.yml` | VMDK disk server on infra cluster | SA kubeconfig |
| `setup-webhook.yml` | Register GitHub webhook via API | SA kubeconfig + github_token |

## Quick Start (Workshop)

For workshop environments, use the all-in-one setup:

```bash
cp ../../workshop-deploy.env.example ../../workshop-deploy.env
vi ../../workshop-deploy.env

# Build fvmw image first
ansible-playbook build.yml -e @../../workshop-deploy.env

# Then run full setup (PVC, disks, pods, MTV providers)
ansible-playbook workshop-setup.yml -e @../../workshop-deploy.env
```

This downloads flat VMDKs from the disk server at
`https://fvmw-disks.apps.ocpv-infra01.dal12.infra.demo.redhat.com/`,
deploys fvmw pods, and creates MTV providers for each user.

## Disk Server (Infra Cluster)

The infra cluster hosts flat VMDK files for workshop clusters to download:

```bash
ansible-playbook disk-server.yml -e @../../local.env
```

URL: `https://fvmw-disks.apps.ocpv-infra01.dal12.infra.demo.redhat.com/`

## Initial Setup

```bash
# From the repo root:
cp local.env.example local.env
vi local.env  # Set cluster_domain, fvmw_users, fvmw_password, etc.
```

### `local.env` variables

| Variable | Required | Description |
|----------|----------|-------------|
| `k8s_kubeconfig` | Yes (after bootstrap) | Path to SA kubeconfig |
| `cluster_domain` | Yes | OCP apps domain (e.g. `apps.cluster.example.com`) |
| `fvmw_namespace` | No (default: `fvmw`) | Target namespace |
| `fvmw_image` | No | Container image (default: internal ImageStream) |
| `fvmw_password` | Yes | vSphere password for all instances |
| `fvmw_users` | Yes | List of user IDs to provision |
| `github_token` | For build/webhook | GitHub token for private repo access |
| `storage_class` | No | PVC storage class (default: `ocs-storagecluster-cephfs`) |

## Step 1: Bootstrap (one-time, cluster-admin)

```bash
oc login <cluster>
cd deploy/ansible
ansible-playbook bootstrap.yml -e @../../local.env
```

Creates:
- `fvmw` namespace
- `fvmw-mgmt` ServiceAccount with minimal ClusterRole (incl `routes/custom-host`)
- Long-lived token and kubeconfig at `~/secrets/fvmw-mgmt.kubeconfig`

Add to `local.env`:
```yaml
k8s_kubeconfig: ~/secrets/fvmw-mgmt.kubeconfig
```

## Step 2: Build Pipeline

```bash
ansible-playbook build.yml -e @../../local.env
```

Creates:
- `fvmw` ImageStream
- `fvmw` BuildConfig (Docker strategy from GitHub repo)
- Source secret for private repo access (if `github_token` set)
- Triggers initial build via ConfigChange

## Step 3: Prepare Disk Images (one-time)

VMDK images must be on the PVC in **monolithicFlat** format for virt-v2v to download.

```bash
# Export from real vCenter
govc export.ovf -vm /DC/vm/database /tmp/export/

# Upload to PVC via helper pod
oc run disk-loader --rm -i --image=registry.access.redhat.com/ubi9 \
  --overrides='{"spec":{"containers":[{"name":"loader","image":"registry.access.redhat.com/ubi9","stdin":true,"volumeMounts":[{"name":"disks","mountPath":"/disks"}]}],"volumes":[{"name":"disks","persistentVolumeClaim":{"claimName":"fvmw-disks"}}]}}' \
  -- bash

# Inside the pod, convert to flat:
dnf install -y qemu-img
qemu-img convert -O vmdk -o subformat=monolithicFlat database.vmdk database-flat.vmdk
```

Required files on PVC for each VM:
- `<name>.vmdk` — streamOptimized (original export)
- `<name>-flat.vmdk` — monolithicFlat (for virt-v2v download)

## Step 4: Deploy Users

```bash
ansible-playbook deploy.yml -e @../../local.env
```

Creates for each user in `fvmw_users`:

**VPX pod (vCenter):**
- `vcenter-<user>` Deployment (VPX mode, `FVMW_HOST=esxi-<user>.<domain>`)
- `vcenter-<user>` Service + Route (edge TLS)

**ESXi pod:**
- `esxi-<user>` Deployment (ESX mode, `FVMW_ESX_MODE=1`)
- `esxi-<user>` Service + Route (edge TLS)

**Shared resources:**
- `fvmw-disks` PVC (ReadWriteMany, 100Gi, CephFS)
- `fvmw-credentials` Secret

## Update / Redeploy

Running any playbook again is idempotent.

```bash
# Trigger a rebuild (picks up latest code from GitHub)
oc start-build fvmw -n fvmw

# Restart pods to pick up new image
oc rollout restart deployment/vcenter-user1 deployment/esxi-user1 -n fvmw
```

## Teardown

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

## GitHub Webhook (optional)

```bash
ansible-playbook setup-webhook.yml -e @../../local.env
```

Requires `github_token` with `admin:repo_hook` scope.

## MTV Provider Setup

After deployment, create an MTV VMware Provider on the target cluster:

- **URL:** `https://vcenter-<user>.<cluster_domain>/sdk`
- **Username:** `administrator@vsphere.local`
- **Password:** (value from `fvmw_password` in local.env)
- **VDDK init image:** leave empty (not needed)
- **SDK endpoint:** `vcenter`

The provider inventory will show datacenter `RS00` with 4 VMs in the `Roadshow` folder.

### Migration Plan

Follow the [roadshow demo instructions](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html):

1. Select VMs: `database-<user>`, `winweb01-<user>`, `winweb02-<user>`
2. Target namespace: `vmexamples-<user>`
3. Network map: Pod Networking
4. Storage map: `ocs-external-storagecluster-ceph-rbd`
5. Start migration — virt-v2v downloads disks, converts, and creates KubeVirt VMs
