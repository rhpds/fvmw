# Workshop CLI Reference

This document provides CLI commands that replace the vCenter GUI steps in the [OpenShift Virtualization Roadshow](https://rhpds.github.io/openshift-virt-roadshow-cnv-multi-user/modules/module-02-mtv.html) migration module.

FVMW does not include a vCenter web GUI. Use these `govc` commands to browse the fake vCenter inventory, or use the MTV provider inventory view in the OpenShift console.

## Setup

```bash
# Install govc (if not already installed)
go install github.com/vmware/govmomi/govc@latest

# Set connection variables (replace <user>, <password>, <cluster>)
export GOVC_URL="https://administrator@vsphere.local:<password>@vcenter-<user>.apps.<cluster>/sdk"
export GOVC_INSECURE=1
```

## Browse the Inventory

These commands replace the "Navigate to VMware vCenter" and "Launch vSphere Client" steps.

```bash
# View datacenter structure (replaces Inventory view)
govc ls /
# Output: /RS00

# View datacenter contents
govc ls /RS00/
# Output:
#   /RS00/vm
#   /RS00/host
#   /RS00/datastore
#   /RS00/network

# List VMs in the Roadshow folder (replaces VMs & Templates view)
govc ls /RS00/vm/Roadshow/
# Output:
#   /RS00/vm/Roadshow/database-user1
#   /RS00/vm/Roadshow/winweb01-user1
#   /RS00/vm/Roadshow/winweb02-user1
#   /RS00/vm/Roadshow/haproxy-user1
```

## View VM Details

These commands replace clicking on a VM in the vSphere Client.

```bash
# Database VM (CentOS 8, PostgreSQL)
govc vm.info /RS00/vm/Roadshow/database-user1
# Shows: guest OS, CPU, memory, power state, host, storage

# Windows web servers
govc vm.info /RS00/vm/Roadshow/winweb01-user1
govc vm.info /RS00/vm/Roadshow/winweb02-user1

# HAProxy load balancer (will NOT be migrated)
govc vm.info /RS00/vm/Roadshow/haproxy-user1
```

## View Infrastructure

```bash
# Hosts & Clusters view
govc ls /RS00/host/
govc ls /RS00/host/vcs-rs-00/

# Datastores view
govc ls /RS00/datastore/

# Networks view
govc ls /RS00/network/
```

## View Disk Details

```bash
# Show disk controller and backing file
govc device.info -vm /RS00/vm/Roadshow/database-user1 'disk-*'
```

## Alternative: MTV Provider Inventory

Instead of CLI commands, students can browse the inventory directly in the OpenShift console:

1. Navigate to **Migration** > **Providers for virtualization**
2. Click on the **vmware** provider
3. Browse **VMs**, **Hosts**, **Networks**, **Datastores** tabs

This provides the same information as the vCenter GUI without needing `govc`.
