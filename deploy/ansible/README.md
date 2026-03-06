# FVMW Ansible Deployment

Ansible playbook to deploy and manage fvmw instances on OpenShift.

## Prerequisites

- `oc` CLI authenticated to your OpenShift cluster
- `ansible` with `kubernetes.core` collection installed:
  ```bash
  ansible-galaxy collection install kubernetes.core
  ```
- VMDK disk images uploaded to the shared PVC

## Setup

```bash
# From the repo root:
cp local.env.example local.env
vi local.env  # Set cluster_domain, fvmw_users, fvmw_password, etc.
```

## Deploy

```bash
cd deploy/ansible
ansible-playbook deploy.yml -e @../../local.env
```

This creates for each user:
- `vcenter-<user>` Deployment (fvmw container)
- `vcenter-<user>` Service (port 8080)
- `vcenter-<user>` Route (edge TLS termination)

Plus shared resources:
- `fvmw-disks` PVC (ReadOnlyMany)
- `fvmw-credentials` Secret

## Update Image

To roll out a new container image:

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_image=quay.io/rhpds/fvmw:v2
```

Running the playbook again is idempotent - it updates existing resources.

## Teardown

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

## MTV Provider Setup

After deployment, each user creates an MTV VMware Provider:

- **URL:** `https://vcenter-<user>.apps.<cluster_domain>/sdk`
- **Username:** `administrator@vsphere.local`
- **Password:** (value from `fvmw_password` in local.env)
- **VDDK init image:** not needed (use non-VDDK transfer via Provider CR annotation)
