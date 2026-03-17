# Using fvmw with the OSP-on-OCP Lab (os-migrate)

This document explains how to use fvmw (Fake VMware) as a mock vCenter
source for the VMware-to-RHOSO migration track in the
[Red Hat OpenStack Services on OpenShift Lab](https://demo.redhat.com).

## Overview

The OSP-on-OCP lab includes a VMware-to-RHOSO migration track that uses
the [os-migrate VMware Migration Kit](https://github.com/os-migrate/vmware-migration-kit)
to migrate VMs from a real vCenter into OpenStack running on OpenShift.

fvmw replaces the real vCenter, and a forked os-migrate replaces the
proprietary VDDK with `nbdkit-curl` for disk transfer over plain HTTPS.
No VMware software is required.

## Architecture

```
                    SOAP API (/sdk)
  os-migrate ──────────────────────────► fvmw (fake vCenter)
  Go binary         REST API (/api/)         │
     │                                       │
     │  nbdkit-curl ◄── HTTPS /folder/ ──────┘
     │       │          (flat VMDK)
     │   nbdcopy
     │       │
     │       ▼
     │  /dev/vdb (Cinder volume)
     │       │
     │  virt-v2v-in-place
     │
     ▼
  OpenStack instance
```

**Data path:** fvmw serves flat VMDKs over HTTPS via its `/folder/`
endpoint. `nbdkit-curl` connects to this URL and exposes the disk via
an NBD socket. `nbdcopy` reads from the socket and writes to a Cinder
volume attached to the conversion host. `virt-v2v-in-place` converts
the disk in-place.

## Prerequisites

1. **OSP-on-OCP lab environment** — order from
   [catalog.demo.redhat.com](https://catalog.demo.redhat.com) with the
   catalog item "Red Hat OpenStack Services on OpenShift Lab"

2. **RHOSO deployed** — complete the Connected Environment track
   (prereqs, operators, network isolation, NFS, control plane, data plane)

3. **OpenStack networking** — public + private networks, router,
   security groups, flavors created

## Components

### fvmw (fake vCenter)

- **Repo:** https://github.com/rhpds/fvmw (branch: `main`)
- **Provides:** SOAP API, REST API (`/api/vcenter/vm/.../hardware/disk`),
  `/folder/` disk serving
- **Deployed as:** Pod on the OCP cluster in the `fvmw` namespace

### os-migrate fork

- **Repo:** https://github.com/rhpds/vmware-migration-kit (branch: `curl-default`)
- **Changes from upstream:**
  - Replaces `nbdkit vddk` with `nbdkit curl` (no VDDK dependency)
  - Constructs `/folder/` URLs from disk backing info
  - Always passes `--destination-is-zero` to nbdcopy
- **DO NOT MERGE upstream** — this fork is specifically for fvmw

## Setup Steps

### 1. Deploy fvmw on the OCP cluster

```bash
# SSH to bastion
ssh lab-user@ssh.ocpv06.rhdp.net -p <PORT>

# Create namespace
oc new-project fvmw

# Create ImageStream and BuildConfig
oc create imagestream fvmw -n fvmw

cat <<EOF | oc apply -n fvmw -f -
apiVersion: build.openshift.io/v1
kind: BuildConfig
metadata:
  name: fvmw
spec:
  source:
    type: Git
    git:
      uri: https://github.com/rhpds/fvmw.git
      ref: main
  strategy:
    type: Docker
    dockerStrategy:
      dockerfilePath: build/Containerfile
  output:
    to:
      kind: ImageStreamTag
      name: fvmw:latest
  triggers:
    - type: ConfigChange
EOF

# Wait for build to complete
oc get builds -n fvmw -w
```

### 2. Create a test disk

The fvmw PVC needs at least one flat VMDK file. Create a small test disk:

```bash
# Create PVC
cat <<EOF | oc apply -n fvmw -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fvmw-disks
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ocs-external-storagecluster-ceph-rbd
EOF

# Create a small test flat VMDK (1MB random data)
cat <<EOF | oc apply -n fvmw -f -
apiVersion: v1
kind: Pod
metadata:
  name: disk-init
spec:
  restartPolicy: Never
  containers:
  - name: init
    image: registry.access.redhat.com/ubi9-minimal
    command: ["sh", "-c", "dd if=/dev/urandom of=/disks/haproxy-flat.vmdk bs=1M count=1 && echo done"]
    volumeMounts:
    - name: disks
      mountPath: /disks
  volumes:
  - name: disks
    persistentVolumeClaim:
      claimName: fvmw-disks
EOF

# Wait for completion, then clean up
oc wait --for=condition=Ready pod/disk-init -n fvmw --timeout=60s || true
sleep 5
oc logs disk-init -n fvmw
oc delete pod disk-init -n fvmw
```

For real migration testing, place actual flat VMDKs (monolithicFlat format)
on the PVC. See the main fvmw README for disk preparation.

### 3. Deploy fvmw pod

```bash
DOMAIN=$(oc get ingresses.config cluster -o jsonpath='{.spec.domain}')
FVMW_IMAGE="image-registry.openshift-image-registry.svc:5000/fvmw/fvmw:latest"

# Credentials secret
oc create secret generic fvmw-credentials -n fvmw \
  --from-literal=username=administrator@vsphere.local \
  --from-literal=password=changeme

# Deployment + Service + Route
cat <<EOF | oc apply -n fvmw -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vcenter-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vcenter-test
  template:
    metadata:
      labels:
        app: vcenter-test
    spec:
      containers:
      - name: fvmw
        image: ${FVMW_IMAGE}
        ports:
        - containerPort: 8080
        env:
        - name: FVMW_USERNAME
          valueFrom:
            secretKeyRef:
              name: fvmw-credentials
              key: username
        - name: FVMW_PASSWORD
          valueFrom:
            secretKeyRef:
              name: fvmw-credentials
              key: password
        - name: FVMW_USER_SUFFIX
          value: "-test"
        - name: FVMW_EXTERNAL_HOST
          value: "vcenter-test.${DOMAIN}"
        resources:
          requests:
            memory: 256Mi
          limits:
            memory: 512Mi
        volumeMounts:
        - name: disks
          mountPath: /disks
      volumes:
      - name: disks
        persistentVolumeClaim:
          claimName: fvmw-disks
---
apiVersion: v1
kind: Service
metadata:
  name: vcenter-test
spec:
  selector:
    app: vcenter-test
  ports:
  - port: 8080
    targetPort: 8080
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: vcenter-test
  annotations:
    haproxy.router.openshift.io/timeout: 300s
spec:
  host: vcenter-test.${DOMAIN}
  to:
    kind: Service
    name: vcenter-test
  port:
    targetPort: 8080
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Allow
EOF

oc rollout status deployment/vcenter-test -n fvmw --timeout=60s
```

### 4. Verify fvmw

```bash
FVMW_URL="https://vcenter-test.${DOMAIN}"

# Test REST API
TOKEN=$(curl -sk -X POST "${FVMW_URL}/api/session" \
  -u 'administrator@vsphere.local:changeme' | tr -d '"')
curl -sk "${FVMW_URL}/api/vcenter/vm" \
  -H "vmware-api-session-id: $TOKEN" | python3 -m json.tool

# Test disk serving
curl -sk -o /dev/null -w "HTTP %{http_code}, %{size_download} bytes\n" \
  -u 'administrator@vsphere.local:changeme' \
  "${FVMW_URL}/folder/haproxy-test/haproxy-flat.vmdk?dcPath=RS00&dsName=workload_share"
```

### 5. Create a conversion host in OpenStack

```bash
oc rsh -n openstack openstackclient

# Create flavor for conversion host
openstack flavor create --ram 4096 --disk 35 --vcpu 2 --public migrate

# Upload a cloud image (CentOS Stream 9 or RHEL 9)
# Copy the image into the openstackclient pod first:
#   oc cp centos9.qcow2 openstack/openstackclient:/home/cloud-admin/
openstack image create centos9-ch --container-format bare \
  --disk-format qcow2 --public --file /home/cloud-admin/centos9.qcow2

# Create keypair (use the lab SSH key)
openstack keypair create --public-key ~/<GUID>key.pub bastion_key

# Launch conversion host
openstack server create --flavor migrate --key-name bastion_key \
  --network private --security-group basic --image centos9-ch conversion-host

# Assign floating IP
openstack floating ip create public --floating-ip-address 192.168.11.154
openstack server add floating ip conversion-host 192.168.11.154
exit
```

### 6. Install tools on conversion host

```bash
ssh -i ~/.ssh/<GUID>key.pem cloud-user@192.168.11.154

# Fix DNS (if needed)
echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf

# Install nbdkit with curl plugin and virt-v2v
sudo dnf install -y nbdkit nbdkit-curl-plugin libnbd virt-v2v
```

### 7. Build the forked os-migrate binary

On the bastion (not the conversion host):

```bash
# Install Go
sudo dnf install -y golang libnbd-devel

# Clone the fork
git clone -b curl-default https://github.com/rhpds/vmware-migration-kit.git
cd vmware-migration-kit/plugins/modules/src/migrate

# Build the migration binary
go build -o ~/migrate .

# Copy to conversion host
scp -i ~/.ssh/<GUID>key.pem ~/migrate cloud-user@192.168.11.154:/home/cloud-user/
```

### 8. Run the migration

Create the migration args file on the bastion:

```bash
cat > ~/migrate-args.json << 'EOF'
{
  "user": "administrator@vsphere.local",
  "password": "changeme",  # pragma: allowlist secret
  "server": "vcenter-test.apps.cluster-GUID.dynamic.redhatworkshops.io",
  "vmname": "haproxy-test",
  "vddkpath": "/RS00/vm/Roadshow",
  "cbtsync": false,
  "skipconversion": true,
  "assumezero": true,
  "usesocks": true,
  "convhostname": "conversion-host",
  "dst_cloud": {
    "auth": {
      "auth_url": "https://keystone-public-openstack.apps.cluster-GUID.dynamic.redhatworkshops.io",
      "username": "admin",
      "project_name": "admin",
      "user_domain_name": "Default",
      "password": "changeme"  # pragma: allowlist secret
    },
    "region_name": "regionOne",
    "interface": "public",
    "identity_api_version": 3,
    "verify": false
  }
}
EOF
```

Replace `GUID` with your lab GUID (e.g. `lkjcd`).

Copy and run on the conversion host:

```bash
scp -i ~/.ssh/<GUID>key.pem ~/migrate-args.json \
  cloud-user@192.168.11.154:/home/cloud-user/

ssh -i ~/.ssh/<GUID>key.pem cloud-user@192.168.11.154 \
  "echo nameserver 8.8.8.8 | sudo tee /etc/resolv.conf > /dev/null && \
   sudo /home/cloud-user/migrate /home/cloud-user/migrate-args.json"
```

**Important:** The migration binary must run as `root` (`sudo`) because
it needs to write to the Cinder block device (`/dev/vdb`).

### Expected output

```
"vCenter connectivity check passed"
"Snapshot created: snapshot-XX"
"Volume ID: <uuid>"
"Volume attached: /dev/vdb"
"Starting nbdkit-curl for disk: https://vcenter-test.../folder/haproxy-test/haproxy-flat.vmdk?..."
"nbdkit is ready."
"█ 100% [****************************************]"
"Disk copied and converted successfully: /dev/vdb"
"VM migrated successfully"
```

## What this proves

1. **fvmw can serve as a mock vCenter for os-migrate** — all SOAP API
   calls (auth, VM lookup, properties, snapshots) work correctly
2. **VDDK is not needed** — `nbdkit-curl` downloads flat VMDKs over
   HTTPS, completely replacing the proprietary VMware library
3. **The full migration pipeline works** — from vCenter API through
   disk transfer to Cinder volume creation in OpenStack
4. **No changes to the lab guide** — students follow the same steps;
   only the container image / binary source changes

## Limitations

- **No CBT (Change Block Tracking)** — incremental sync is not supported
  with the curl plugin. Full disk copies only.
- **No real VM data** — fvmw serves test/demo disk images, not real
  VMware VMs. The migrated OpenStack instance won't boot unless real
  disk images are provided.
- **V2V conversion skipped** — set `skipconversion: false` and provide
  real disk images to test the full virt-v2v-in-place pipeline.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `EOF` on CreateSnapshot | fvmw pod crashed (type assertion panic) | Ensure fvmw version includes the AddHandler fix |
| 404 on `/folder/` URL | Disk path missing VM name prefix | Ensure os-migrate fork includes the VM name fix |
| `File exists` from nbdcopy | Running as non-root user | Run with `sudo` |
| `volume already exists` | Previous failed run left a volume | Delete with `openstack volume delete --force <id>` |
| DNS resolution failure on conversion host | No DNS configured | `echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf` |
| Route timeout on SOAP calls | Default 30s route timeout | Add annotation `haproxy.router.openshift.io/timeout: 300s` |
