package inventory

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/esx"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

// Build creates a vcsim Model and populates it with the inventory defined in cfg.
func Build(cfg *Config) (*simulator.Model, error) {
	if cfg.ESXMode {
		return buildESX(cfg)
	}
	return buildVPX(cfg)
}

// buildVPX creates a vCenter (VPX) model with datacenter/cluster/host hierarchy.
func buildVPX(cfg *Config) (*simulator.Model, error) {
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

	client, err := vim25.NewClient(context.Background(), model.Service)
	if err != nil {
		return nil, fmt.Errorf("creating vim25 client: %w", err)
	}
	root := object.NewRootFolder(client)

	dc, err := root.CreateDatacenter(context.Background(), cfg.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("creating datacenter %q: %w", cfg.Datacenter, err)
	}
	log.Printf("Created datacenter: %s", cfg.Datacenter)

	folders, err := dc.Folders(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting DC folders: %w", err)
	}

	clusterSpec := types.ClusterConfigSpecEx{}
	cluster, err := folders.HostFolder.CreateCluster(context.Background(), cfg.Cluster, clusterSpec)
	if err != nil {
		return nil, fmt.Errorf("creating cluster %q: %w", cfg.Cluster, err)
	}
	log.Printf("Created cluster: %s", cfg.Cluster)

	hostSpec := types.HostConnectSpec{HostName: cfg.Host}
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

	pool, err := cluster.ResourcePool(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting resource pool: %w", err)
	}

	vmFolder := folders.VmFolder
	if cfg.Folder != "" {
		vmFolder, err = folders.VmFolder.CreateFolder(context.Background(), cfg.Folder)
		if err != nil {
			return nil, fmt.Errorf("creating VM folder %q: %w", cfg.Folder, err)
		}
		log.Printf("Created VM folder: %s", cfg.Folder)
	}

	if err := createVMs(cfg, client, vmFolder, pool, host, datastoreDir); err != nil {
		return nil, err
	}

	return model, nil
}

// buildESX creates an ESXi (HostAgent) model with a flat VM structure.
// Used as the ESXi host endpoint for libvirt's vpx:// driver.
func buildESX(cfg *Config) (*simulator.Model, error) {
	model := &simulator.Model{
		ServiceContent: esx.ServiceContent,
		RootFolder:     esx.RootFolder,
		Datacenter:     0,
		Host:           0,
		Machine:        0,
	}

	if err := model.Create(); err != nil {
		return nil, fmt.Errorf("creating ESX model: %w", err)
	}

	client, err := vim25.NewClient(context.Background(), model.Service)
	if err != nil {
		return nil, fmt.Errorf("creating vim25 client: %w", err)
	}

	// ESX model has a pre-created host and datacenter
	// Find the existing host
	finder := object.NewSearchIndex(client)
	dcRef, err := finder.FindByInventoryPath(context.Background(), "/ha-datacenter")
	if err != nil || dcRef == nil {
		return nil, fmt.Errorf("finding ESX datacenter: %w", err)
	}
	dc := object.NewDatacenter(client, dcRef.Reference())

	folders, err := dc.Folders(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting ESX folders: %w", err)
	}

	// ESX model has a pre-existing host at a known MOR
	host := object.NewHostSystem(client, types.ManagedObjectReference{
		Type: "HostSystem", Value: "ha-host",
	})

	// Create datastore
	datastoreDir, err := os.MkdirTemp("", "fvmw-esx-datastore-")
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
	log.Printf("ESX mode: datastore %s -> %s", cfg.Datastore, datastoreDir)

	// Get the resource pool from the compute resource
	cr := object.NewComputeResource(client, types.ManagedObjectReference{
		Type: "ComputeResource", Value: "ha-compute-res",
	})
	pool, err := cr.ResourcePool(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting ESX resource pool: %w", err)
	}

	if err := createVMs(cfg, client, folders.VmFolder, pool, host, datastoreDir); err != nil {
		return nil, err
	}

	log.Println("ESX mode: ready (apiType=HostAgent)")
	return model, nil
}

// createVMs creates the configured VMs in the given folder.
func createVMs(cfg *Config, client *vim25.Client, vmFolder *object.Folder, pool *object.ResourcePool, host *object.HostSystem, datastoreDir string) error {
	for _, vmCfg := range cfg.VMs {
		dsPath := fmt.Sprintf("[%s]", cfg.Datastore)
		vmName := vmCfg.Name + cfg.UserSuffix

		guestID := vmCfg.GuestID
		if guestID == "" {
			guestID = string(types.VirtualMachineGuestOsIdentifierOtherGuest)
		}

		firmware := vmCfg.Firmware
		if firmware == "" {
			firmware = string(types.GuestOsDescriptorFirmwareTypeBios)
		}

		spec := types.VirtualMachineConfigSpec{
			Name:     vmName,
			GuestId:  guestID,
			NumCPUs:  vmCfg.NumCPUs,
			MemoryMB: int64(vmCfg.MemoryMB),
			Firmware: firmware,
			BootOptions: &types.VirtualMachineBootOptions{
				EfiSecureBootEnabled: types.NewBool(false),
			},
			Files: &types.VirtualMachineFileInfo{
				VmPathName: dsPath,
			},
		}

		task, err := vmFolder.CreateVM(context.Background(), spec, pool, host)
		if err != nil {
			return fmt.Errorf("creating VM %q: %w", vmName, err)
		}
		result, err := task.WaitForResult(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("waiting for VM %q creation: %w", vmName, err)
		}
		vmRef := result.Result.(types.ManagedObjectReference)
		vm := object.NewVirtualMachine(client, vmRef)

		// Create stub VMDK in datastore for vcsim validation
		stubPath := filepath.Join(datastoreDir, vmCfg.Disk)
		if err := os.MkdirAll(filepath.Dir(stubPath), 0755); err != nil {
			return fmt.Errorf("creating stub dir for %q: %w", vmCfg.Disk, err)
		}
		if err := os.WriteFile(stubPath, []byte("# fvmw stub"), 0644); err != nil {
			return fmt.Errorf("creating stub vmdk for %q: %w", vmCfg.Disk, err)
		}

		vmDiskPath := fmt.Sprintf("[%s] %s", cfg.Datastore, vmCfg.Disk)

		scsiCtrlKey := int32(1000)
		diskUnitNumber := int32(0)

		var scsiController types.BaseVirtualDevice
		switch vmCfg.DiskController {
		case "lsilogic-sas":
			scsiController = &types.VirtualLsiLogicSASController{
				VirtualSCSIController: types.VirtualSCSIController{
					VirtualController: types.VirtualController{
						VirtualDevice: types.VirtualDevice{Key: scsiCtrlKey},
						BusNumber:     0,
					},
					SharedBus: types.VirtualSCSISharingNoSharing,
				},
			}
		default:
			scsiController = &types.ParaVirtualSCSIController{
				VirtualSCSIController: types.VirtualSCSIController{
					VirtualController: types.VirtualController{
						VirtualDevice: types.VirtualDevice{Key: scsiCtrlKey},
						BusNumber:     0,
					},
					SharedBus: types.VirtualSCSISharingNoSharing,
				},
			}
		}

		diskSpec := types.VirtualMachineConfigSpec{
			DeviceChange: []types.BaseVirtualDeviceConfigSpec{
				&types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationAdd,
					Device:    scsiController,
				},
				&types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationAdd,
					Device: &types.VirtualDisk{
						VirtualDevice: types.VirtualDevice{
							Key:           -1,
							ControllerKey: scsiCtrlKey,
							UnitNumber:    &diskUnitNumber,
							Backing: &types.VirtualDiskFlatVer2BackingInfo{
								VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
									FileName: vmDiskPath,
								},
								DiskMode: string(types.VirtualDiskModePersistent),
								Sharing:  string(types.VirtualDiskSharingSharingNone),
							},
						},
						CapacityInBytes: diskCapacityBytes(vmCfg.DiskSizeGB),
					},
				},
			},
		}

		reconfigTask, err := vm.Reconfigure(context.Background(), diskSpec)
		if err != nil {
			return fmt.Errorf("adding disk to VM %q: %w", vmName, err)
		}
		if err := reconfigTask.Wait(context.Background()); err != nil {
			return fmt.Errorf("waiting for disk add on VM %q: %w", vmName, err)
		}

		// Write VMX content to the .vmx file that vcsim created.
		// libvirt's ESX driver downloads this via the /folder/ endpoint
		// and parses it to build the domain XML.
		vmxPath := filepath.Join(datastoreDir, vmName, vmName+".vmx")
		vmxContent := generateVMX(vmName, guestID, vmCfg)
		if err := os.WriteFile(vmxPath, []byte(vmxContent), 0600); err != nil {
			log.Printf("Warning: could not write VMX for %s: %v", vmName, err)
		}

		log.Printf("Created VM: %s (guest=%s, disk=%s, %dGB)", vmName, guestID, vmCfg.Disk, vmCfg.DiskSizeGB)
	}

	return nil
}

func generateVMX(vmName, guestID string, cfg VMConfig) string {
	scsiType := "pvscsi"
	if cfg.DiskController == "lsilogic-sas" {
		scsiType = "lsilogic"
	}

	return fmt.Sprintf(`.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "19"
displayName = "%s"
guestOS = "%s"
memSize = "%d"
numvcpus = "%d"
firmware = "%s"
scsi0.present = "TRUE"
scsi0.virtualDev = "%s"
scsi0:0.present = "TRUE"
scsi0:0.fileName = "%s"
scsi0:0.deviceType = "scsi-hardDisk"
ethernet0.present = "TRUE"
ethernet0.virtualDev = "vmxnet3"
ethernet0.connectionType = "bridged"
ethernet0.networkName = "VM Network"
`, vmName, guestID, cfg.MemoryMB, cfg.NumCPUs, cfg.Firmware, scsiType, cfg.Disk)
}

func diskCapacityBytes(sizeGB int64) int64 {
	if sizeGB <= 0 {
		sizeGB = 10
	}
	return sizeGB * 1024 * 1024 * 1024
}
