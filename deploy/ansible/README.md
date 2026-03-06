# FVMW Ansible Deployment

Ansible playbooks to deploy and manage fvmw instances on OpenShift.

## Prerequisites

- `ansible` with `kubernetes.core` collection:
  ```bash
  ansible-galaxy collection install kubernetes.core
  ```
- `oc` CLI (for initial bootstrap only)
- VMDK disk images uploaded to the shared PVC

## Playbooks

| Playbook | Purpose | Requires |
|----------|---------|----------|
| `bootstrap.yml` | One-time namespace, SA, RBAC, kubeconfig setup | cluster-admin |
| `build.yml` | BuildConfig, ImageStream, GitHub webhook | SA kubeconfig |
| `deploy.yml` | Per-user Deployment, Service, Route | SA kubeconfig |
| `setup-webhook.yml` | Register GitHub webhook via API | SA kubeconfig + github_token |

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
- `fvmw-mgmt` ServiceAccount with minimal ClusterRole
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

The image is built inside OCP and pushed to the internal registry.

## Step 3: Deploy Users

```bash
ansible-playbook deploy.yml -e @../../local.env
```

Creates for each user in `fvmw_users`:
- `vcenter-<user>` Deployment (fvmw container with `FVMW_USER_SUFFIX=-<user>`)
- `vcenter-<user>` Service (port 8080)
- `vcenter-<user>` Route (edge TLS at `vcenter-<user>.<cluster_domain>`)

Plus shared resources:
- `fvmw-disks` PVC (ReadWriteMany, CephFS)
- `fvmw-credentials` Secret (username/password from local.env)

## Update / Redeploy

Running any playbook again is idempotent. To roll out a new image after a rebuild:

```bash
# Trigger a rebuild (picks up latest code from GitHub)
oc start-build fvmw -n fvmw

# Or set a specific image
ansible-playbook deploy.yml -e @../../local.env -e fvmw_image=quay.io/rhpds/fvmw:v2
```

## Teardown

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

## GitHub Webhook (optional)

To auto-build on push:

```bash
ansible-playbook setup-webhook.yml -e @../../local.env
```

Requires `github_token` with `admin:repo_hook` scope.

## MTV Provider Setup

After deployment, each user creates an MTV VMware Provider:

- **URL:** `https://vcenter-<user>.<cluster_domain>/sdk`
- **Username:** `administrator@vsphere.local`
- **Password:** (value from `fvmw_password` in local.env)
- **VDDK init image:** not needed (use non-VDDK transfer via Provider CR annotation)

The provider inventory will show datacenter `RS00` with 4 VMs in the `Roadshow` folder.
