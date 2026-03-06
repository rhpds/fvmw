package inventory

import (
	"context"
	"fmt"
	"log"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

// Build creates a vcsim Model and populates it with the inventory defined in cfg.
func Build(cfg *Config) (*simulator.Model, error) {
	model := &simulator.Model{
		ServiceContent: vpx.ServiceContent,
		RootFolder:     vpx.RootFolder,
		Datacenter:     0,
		Cluster:        0,
		Host:           0,
		Machine:        0,
	}

	if err := model.Create(); err != nil {
		return nil, fmt.Errorf("creating vcsim model: %w", err)
	}

	// Create an in-memory vim25 client via the Service's RoundTripper
	client, err := vim25.NewClient(context.Background(), model.Service)
	if err != nil {
		return nil, fmt.Errorf("creating vim25 client: %w", err)
	}
	root := object.NewRootFolder(client)

	// Create datacenter
	dc, err := root.CreateDatacenter(context.Background(), cfg.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("creating datacenter %q: %w", cfg.Datacenter, err)
	}
	log.Printf("Created datacenter: %s", cfg.Datacenter)

	folders, err := dc.Folders(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting DC folders: %w", err)
	}

	// Create cluster
	clusterSpec := types.ClusterConfigSpecEx{}
	cluster, err := folders.HostFolder.CreateCluster(context.Background(), cfg.Cluster, clusterSpec)
	if err != nil {
		return nil, fmt.Errorf("creating cluster %q: %w", cfg.Cluster, err)
	}
	log.Printf("Created cluster: %s", cfg.Cluster)

	// Add host to cluster
	hostSpec := types.HostConnectSpec{
		HostName: cfg.Host,
	}
	task, err := cluster.AddHost(context.Background(), hostSpec, true, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("adding host %q: %w", cfg.Host, err)
	}
	result, err := task.WaitForResult(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("waiting for host add: %w", err)
	}
	hostRef := result.Result.(types.ManagedObjectReference)
	host := object.NewHostSystem(client, hostRef)
	log.Printf("Added host: %s", cfg.Host)

	// Create datastore (local path mapped to disk path)
	datastoreSystem, err := host.ConfigManager().DatastoreSystem(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting datastore system: %w", err)
	}

	_, err = datastoreSystem.CreateLocalDatastore(context.Background(), cfg.Datastore, cfg.DiskPath)
	if err != nil {
		return nil, fmt.Errorf("creating datastore %q: %w", cfg.Datastore, err)
	}
	log.Printf("Created datastore: %s -> %s", cfg.Datastore, cfg.DiskPath)

	// Get the resource pool for the cluster
	pool, err := cluster.ResourcePool(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting resource pool: %w", err)
	}

	// Create VMs
	for _, vmCfg := range cfg.VMs {
		dsPath := fmt.Sprintf("[%s]", cfg.Datastore)

		guestID := vmCfg.GuestID
		if guestID == "" {
			guestID = string(types.VirtualMachineGuestOsIdentifierOtherGuest)
		}

		spec := types.VirtualMachineConfigSpec{
			Name:     vmCfg.Name,
			GuestId:  guestID,
			NumCPUs:  vmCfg.NumCPUs,
			MemoryMB: int64(vmCfg.MemoryMB),
			Files: &types.VirtualMachineFileInfo{
				VmPathName: dsPath,
			},
		}

		task, err := folders.VmFolder.CreateVM(context.Background(), spec, pool, host)
		if err != nil {
			return nil, fmt.Errorf("creating VM %q: %w", vmCfg.Name, err)
		}
		result, err := task.WaitForResult(context.Background(), nil)
		if err != nil {
			return nil, fmt.Errorf("waiting for VM %q creation: %w", vmCfg.Name, err)
		}
		vmRef := result.Result.(types.ManagedObjectReference)
		vm := object.NewVirtualMachine(client, vmRef)

		// Add a virtual disk backed by the VMDK file.
		// Use empty FileOperation so vcsim expects the file to already exist
		// (we serve the real VMDK from the shared disk path via ExportVm).
		vmDiskPath := fmt.Sprintf("[%s] %s", cfg.Datastore, vmCfg.Disk)

		diskSpec := types.VirtualMachineConfigSpec{
			DeviceChange: []types.BaseVirtualDeviceConfigSpec{
				&types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationAdd,
					Device: &types.VirtualDisk{
						VirtualDevice: types.VirtualDevice{
							Backing: &types.VirtualDiskFlatVer2BackingInfo{
								VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
									FileName: vmDiskPath,
								},
								DiskMode: string(types.VirtualDiskModePersistent),
							},
						},
						CapacityInBytes: 10 * 1024 * 1024 * 1024, // 10 GB default
					},
				},
			},
		}

		reconfigTask, err := vm.Reconfigure(context.Background(), diskSpec)
		if err != nil {
			return nil, fmt.Errorf("adding disk to VM %q: %w", vmCfg.Name, err)
		}
		if err := reconfigTask.Wait(context.Background()); err != nil {
			return nil, fmt.Errorf("waiting for disk add on VM %q: %w", vmCfg.Name, err)
		}

		// Power on the VM
		powerTask, err := vm.PowerOn(context.Background())
		if err != nil {
			return nil, fmt.Errorf("powering on VM %q: %w", vmCfg.Name, err)
		}
		_ = powerTask.Wait(context.Background())

		log.Printf("Created VM: %s (guest=%s, disk=%s)", vmCfg.Name, guestID, vmCfg.Disk)
	}

	return model, nil
}
