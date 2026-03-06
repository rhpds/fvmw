package inventory

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

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

	// Create datastore backed by a writable temp directory.
	// vcsim needs to write VM metadata (.vmx files, directories) into the
	// datastore path. The actual VMDK files are served from DiskPath
	// (which may be a read-only PVC mount) via the ExportVm handler.
	datastoreDir, err := os.MkdirTemp("", "fvmw-datastore-")
	if err != nil {
		return nil, fmt.Errorf("creating temp datastore dir: %w", err)
	}

	datastoreSystem, err := host.ConfigManager().DatastoreSystem(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting datastore system: %w", err)
	}

	_, err = datastoreSystem.CreateLocalDatastore(context.Background(), cfg.Datastore, datastoreDir)
	if err != nil {
		return nil, fmt.Errorf("creating datastore %q: %w", cfg.Datastore, err)
	}
	log.Printf("Created datastore: %s -> %s (disks served from %s)", cfg.Datastore, datastoreDir, cfg.DiskPath)

	// Get the resource pool for the cluster
	pool, err := cluster.ResourcePool(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting resource pool: %w", err)
	}

	// Create VM folder if specified
	vmFolder := folders.VmFolder
	if cfg.Folder != "" {
		vmFolder, err = folders.VmFolder.CreateFolder(context.Background(), cfg.Folder)
		if err != nil {
			return nil, fmt.Errorf("creating VM folder %q: %w", cfg.Folder, err)
		}
		log.Printf("Created VM folder: %s", cfg.Folder)
	}

	// Create VMs
	for _, vmCfg := range cfg.VMs {
		dsPath := fmt.Sprintf("[%s]", cfg.Datastore)

		vmName := vmCfg.Name + cfg.UserSuffix

		guestID := vmCfg.GuestID
		if guestID == "" {
			guestID = string(types.VirtualMachineGuestOsIdentifierOtherGuest)
		}

		spec := types.VirtualMachineConfigSpec{
			Name:     vmName,
			GuestId:  guestID,
			NumCPUs:  vmCfg.NumCPUs,
			MemoryMB: int64(vmCfg.MemoryMB),
			Files: &types.VirtualMachineFileInfo{
				VmPathName: dsPath,
			},
		}

		task, err := vmFolder.CreateVM(context.Background(), spec, pool, host)
		if err != nil {
			return nil, fmt.Errorf("creating VM %q: %w", vmName, err)
		}
		result, err := task.WaitForResult(context.Background(), nil)
		if err != nil {
			return nil, fmt.Errorf("waiting for VM %q creation: %w", vmName, err)
		}
		vmRef := result.Result.(types.ManagedObjectReference)
		vm := object.NewVirtualMachine(client, vmRef)

		// Create a stub VMDK file in the datastore so vcsim can find it.
		// The real VMDK is served from DiskPath via the ExportVm handler.
		stubPath := filepath.Join(datastoreDir, vmCfg.Disk)
		if err := os.MkdirAll(filepath.Dir(stubPath), 0755); err != nil {
			return nil, fmt.Errorf("creating stub dir for %q: %w", vmCfg.Disk, err)
		}
		if err := os.WriteFile(stubPath, []byte("# fvmw stub"), 0644); err != nil {
			return nil, fmt.Errorf("creating stub vmdk for %q: %w", vmCfg.Disk, err)
		}

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
			return nil, fmt.Errorf("adding disk to VM %q: %w", vmName, err)
		}
		if err := reconfigTask.Wait(context.Background()); err != nil {
			return nil, fmt.Errorf("waiting for disk add on VM %q: %w", vmName, err)
		}

		// Power on the VM
		powerTask, err := vm.PowerOn(context.Background())
		if err != nil {
			return nil, fmt.Errorf("powering on VM %q: %w", vmName, err)
		}
		_ = powerTask.Wait(context.Background())

		log.Printf("Created VM: %s (guest=%s, disk=%s)", vmName, guestID, vmCfg.Disk)
	}

	return model, nil
}
