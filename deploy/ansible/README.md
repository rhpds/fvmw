# FVMW Ansible Deployment

Ansible playbooks to deploy fvmw (Fake VMware) for MTV migration demos.

## Prerequisites

- `ansible` with `kubernetes.core` collection:
  ```bash
  ansible-galaxy collection install kubernetes.core
  ```
- A disk server hosting flat VMDK images (see Step 1 below)
- Workshop cluster with MTV and OpenShift Virtualization installed

## Playbooks

| Playbook | Purpose | Target | Requires |
|----------|---------|--------|----------|
| `disk-server.yml` | Host VMDK images for workshops | Infra cluster | cluster-admin |
| `workshop-setup.yml` | **Full workshop setup** | Workshop cluster | cluster-admin |
| `bootstrap.yml` | SA, RBAC, kubeconfig (manual ops) | Any cluster | cluster-admin |
| `build.yml` | BuildConfig, ImageStream | Any cluster | SA kubeconfig |
| `deploy.yml` | Per-user pods, services, routes | Any cluster | SA kubeconfig |
| `setup-webhook.yml` | GitHub webhook for auto-builds | Any cluster | SA kubeconfig + github_token |

---

## Step 1: Disk Server (Infra Admin, One-Time)

The disk server hosts flat VMDK images that workshop clusters download.
Run this once on a persistent infra cluster.

**Prerequisites:** cluster-admin on the infra cluster and access to a real
vCenter with the source VMs.

```bash
cp ../../local.env.example ../../local.env
vi ../../local.env
# Set: k8s_kubeconfig, cluster_domain, source_vcenter_url/user/password

ansible-playbook disk-server.yml -e @../../local.env
```

This playbook:
1. Creates a 100Gi CephFS PVC
2. Exports VMDKs from the real vCenter via `govc export.ovf`
3. Converts to monolithicFlat format with `qemu-img`
4. Deploys nginx to serve the flat VMDKs via HTTPS

Note the output URL — workshop clusters need it as `disk_source_url`.

---

## Step 2: Workshop Setup (Per Workshop)

`workshop-setup.yml` is the **only playbook needed** for each workshop.
It does everything: build, disk download, pods, and MTV configuration.

```bash
cp ../../workshop-deploy.env.example ../../workshop-deploy.env
vi ../../workshop-deploy.env
# Set: k8s_kubeconfig, cluster_domain, fvmw_password, fvmw_users, disk_source_url

cd deploy/ansible
ansible-playbook workshop-setup.yml -e @../../workshop-deploy.env
```

This single command:
1. Builds the fvmw container image from the GitHub repo
2. Creates the shared 100Gi PVC and downloads flat VMDKs from the disk server
3. Deploys vcenter + esxi pod pairs per user
4. Creates MTV providers, network/storage mappings, and target namespaces per user

After completion, students can follow the
[roadshow migration instructions](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html).

### `workshop-deploy.env` variables

| Variable | Required | Description |
|----------|----------|-------------|
| `k8s_kubeconfig` | Yes | Kubeconfig with cluster-admin access |
| `cluster_domain` | Yes | Workshop cluster apps domain |
| `fvmw_password` | Yes | vSphere password for all instances |
| `fvmw_users` | Yes | List of user IDs to provision |
| `disk_source_url` | Yes | URL of the disk server from Step 1 |
| `github_token` | If private repo | GitHub token for repo access |
| `storage_class` | No | PVC storage class (default: CephFS) |
| `fvmw_namespace` | No | Namespace (default: `fvmw`) |

---

## Individual Playbooks

The following playbooks are for manual operations when you need more control.

### Bootstrap (SA + RBAC)

```bash
oc login <cluster>
ansible-playbook bootstrap.yml -e @../../local.env
```

Creates a `fvmw-mgmt` ServiceAccount with minimal ClusterRole and generates
a kubeconfig at `~/secrets/fvmw-mgmt.kubeconfig`.

### Build

```bash
ansible-playbook build.yml -e @../../local.env
```

Creates BuildConfig + ImageStream. Triggers initial build from GitHub.

### Deploy

```bash
ansible-playbook deploy.yml -e @../../local.env
```

Deploys per-user VPX + ESXi pods, services, routes, shared PVC, credentials.

### Teardown

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

---

## MTV Provider (created by workshop-setup)

Each user gets a pre-configured VMware provider:

- **URL:** `https://vcenter-<user>.<cluster_domain>/sdk`
- **Username:** `administrator@vsphere.local`
- **Password:** (from `fvmw_password`)
- **Inventory:** Datacenter `RS00`, folder `Roadshow`, 4 VMs per user

### Migration Plan

1. Select VMs: `database-<user>`, `winweb01-<user>`, `winweb02-<user>`
2. Target namespace: `vmexamples-<user>`
3. Network map: Pod Networking
4. Storage map: `ocs-external-storagecluster-ceph-rbd`
5. Start migration
