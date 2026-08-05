# fvmw Helm chart

Deploys the shared `fvmw` endpoint (Deployment, Service, Route, PVC, credentials
Secret) to a new OpenShift cluster.

## Install

```bash
helm install fvmw charts/fvmw \
  --namespace fvmw --create-namespace \
  --set clusterDomain=apps.mycluster.example.com \
  --set fvmw.password=changeme
```

Or set an explicit hostname instead of `clusterDomain` (which just derives
`vcenter.<clusterDomain>`):

```bash
helm install fvmw charts/fvmw \
  --namespace fvmw --create-namespace \
  --set host=vcenter.apps.mycluster.example.com
```

This creates an empty PVC for the VMDK disk images — populate it before
migrating VMs (see `deploy/ansible/prepare-disks.yml`), or point at an
already-populated PVC with `--set disks.existingClaim=<name>`.

## Upgrade

```bash
helm upgrade fvmw charts/fvmw -n fvmw --reuse-values \
  --set replicaCount=10
```

## Key values

| Value | Default | Description |
|---|---|---|
| `host` | _(none)_ | Full hostname fvmw is reached at. Required unless `clusterDomain` is set. |
| `clusterDomain` | `""` | Convenience: derives `host` as `vcenter.<clusterDomain>`. |
| `replicaCount` | `6` | fvmw pod replica count. See comment in `values.yaml` for scaling guidance. |
| `fvmw.numUsers` | `60` | Number of simulated users (user1..userN) to generate. |
| `fvmw.password` | `changeme` | Shared login password for all users. |
| `fvmw.existingSecret` | `""` | Use an existing Secret's `password` key instead. |
| `disks.size` | `100Gi` | Size of the shared ReadWriteMany PVC. |
| `disks.storageClassName` | `""` | Must support ReadWriteMany (e.g. CephFS). |
| `disks.existingClaim` | `""` | Reuse an existing PVC instead of creating one. |
| `route.enabled` | `true` | Create an OpenShift Route. Disable for plain Kubernetes. |
| `route.timeout` | `180s` | Router timeout; raise if Provider syncs are slow under load. |

See `values.yaml` for the full list.

## Uninstall

```bash
helm uninstall fvmw -n fvmw
```

Note: this does not delete the PVC's underlying data if you don't want it
removed, unset `disks.create` or manage the PVC outside the chart via
`disks.existingClaim`.
