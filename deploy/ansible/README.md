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
| `prepare-disks.yml` | Populate existing PVC with VM disks | Any cluster | cluster-admin |
| `deploy.yml` | Shared endpoint + per-user auth users | Any cluster | SA kubeconfig |
| `deploy-ui.yml` | Deploy and verify UI route (`/ui/`) | Any cluster | SA kubeconfig |
| `bootstrap.yml` | SA, RBAC, kubeconfig (manual ops) | Any cluster | cluster-admin |
| `build.yml` | BuildConfig, ImageStream | Any cluster | SA kubeconfig |
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

## Step 2: Deploy fvmw and Prepare Disks

```bash
cp ../../local.env.example ../../local.env
vi ../../local.env
# Set: k8s_kubeconfig, cluster_domain, fvmw_password, fvmw_num_users, disk_source_url

cd deploy/ansible
ansible-playbook build.yml -e @../../local.env
ansible-playbook deploy.yml -e @../../local.env
ansible-playbook prepare-disks.yml -e @../../local.env
```

This:
1. Builds the fvmw container image from the GitHub repo
2. Deploys one shared `vcenter.<domain>` endpoint (Route) with generated users (`user1..userN`)
3. Populates the shared PVC with flat VMDKs from the disk server

## Step 3: MTV Provider and Migration Plan (Per User)

Create a `Provider` per user pointing at the Route hostname, and leave
`vddkInitImage` unset (see "Do not configure VDDK" below):

```bash
oc create secret generic vmware-<user> -n <mtv-namespace> \
  --from-literal=user=<user> \
  --from-literal=password=<fvmw_password> \
  --type=Opaque

oc apply -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: Provider
metadata:
  name: vmware-<user>
  namespace: <mtv-namespace>
spec:
  type: vsphere
  url: https://vcenter.<cluster_domain>/sdk
  secret:
    name: vmware-<user>
    namespace: <mtv-namespace>
EOF
```

Then create a `Plan` selecting `database-<user>`, `winweb01-<user>`,
`winweb02-<user>`, `haproxy-<user>` from datacenter `RS00` / folder `Roadshow`,
and a `Migration` referencing that plan. See the
[roadshow migration instructions](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html)
for the full student-facing GUI flow (`vddkInitImage` guidance above still applies).

---

## Individual Playbooks

The following playbooks are for manual operations when you need more control.

### Prepare Disks Only

```bash
ansible-playbook prepare-disks.yml -e @../../local.env -e disk_source_mode=s3
```

Source modes:
- `disk_source_mode=http` (requires `disk_source_url`)
- `disk_source_mode=s3` (requires `fvmw_datavolume_s3_*` vars and per-VM `datavolume_s3_url`)
- `disk_source_mode=vcenter` (requires `source_vcenter_*` vars)

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

Deploys one shared fvmw endpoint (`vcenter.<cluster_domain>`) with generated users
(`user1..userN`) based on `fvmw_num_users`.

### Deploy + UI Route Check

```bash
ansible-playbook deploy-ui.yml -e @../../local.env
```

Deploys fvmw and verifies:
- `https://vcenter.<cluster_domain>/about`
- `https://vcenter.<cluster_domain>/ui/`

### Teardown

```bash
ansible-playbook deploy.yml -e @../../local.env -e fvmw_action=teardown
```

---

## Do not configure VDDK (`vddkInitImage`)

`vcsim` (the simulator `fvmw` is built on) only implements the vSphere control-plane
SOAP API and the `HttpNfcLease` bulk-export API — it does **not** implement VMware's
raw NFC binary wire protocol that VDDK needs to actually read disk bytes
(`VixDiskLib_Open`/`_Read`). If the Provider has `settings.vddkInitImage` set, virt-v2v
uses `-it vddk`, which will fail during the `ConvertGuest` phase with a generic
`VDDK Error 13 (You do not have access rights to this file)` — none of `san`/`hotadd`/
`nbd`/`nbdssl` transport modes have anything real to connect to.

Without `vddkInitImage`, virt-v2v instead uses `libvirt`'s native vSphere disk access,
which does a plain HTTPS `GET`/`HEAD` against `fvmw`'s built-in `/folder/` datastore
endpoint — this is exactly what `fvmw` implements, and end-to-end migration (including
guest conversion) works correctly this way. **Leave `vddkInitImage` unset.**

### Datastore name must not contain `_` or other non-alphanumeric characters

`libvirt`'s ESX driver double URL-encodes the datastore name when building `/folder/`
request query strings (e.g. `_` becomes `%255f` instead of `%5f`), which `fvmw`'s
`/folder/` handler (a single-decode-pass `net/http` mux) then fails to match, returning
`404`. This is a `libvirt` bug/quirk, not something under `fvmw`'s control, and it
affects any non-alphanumeric character in the datastore name (not just `_`). Keep
`datastore` in `config/default.yaml` (or wherever it's configured) purely alphanumeric,
e.g. `workloadshare` rather than `workload_share` or `workload-share`.
