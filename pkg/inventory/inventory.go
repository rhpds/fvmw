package inventory

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/esx"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
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

	renameDefaultNetwork(model, cfg.Network)

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

	renameDefaultNetwork(model, cfg.Network)

	if err := createVMs(cfg, client, folders.VmFolder, pool, host, datastoreDir); err != nil {
		return nil, err
	}

	log.Println("ESX mode: ready (apiType=HostAgent)")
	return model, nil
}

// renameDefaultNetwork renames vcsim's auto-created default "VM Network"
// port group to the configured network name. VM NIC devices reference a
// network by name (resolved to a moref by vcsim at Reconfigure time), and
// MTV/forklift's inventory reports that same name as the source network for
// migration NetworkMaps, so this keeps both consistent with what operators
// expect to see (e.g. "segment-migrating-to-ocpvirt") instead of the vcsim
// default.
func renameDefaultNetwork(model *simulator.Model, name string) {
	if name == "" {
		return
	}
	ctx := model.Service.Context
	nets := ctx.Map.All("Network")
	if len(nets) == 0 {
		return
	}
	if n, ok := nets[0].(*mo.Network); ok {
		n.Name = name
		n.Entity().Name = name
	}
}

// createVMs creates the configured VMs in the given folder.
func createVMs(cfg *Config, client *vim25.Client, vmFolder *object.Folder, pool *object.ResourcePool, host *object.HostSystem, datastoreDir string) error {
	suffixes := cfg.VMNameSuffixes()
	for _, suffix := range suffixes {
		for _, vmCfg := range cfg.VMs {
			dsPath := fmt.Sprintf("[%s]", cfg.Datastore)
			vmName := vmCfg.Name + suffix

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
			if writeErr := os.WriteFile(stubPath, []byte("# fvmw stub"), 0600); writeErr != nil {
				return fmt.Errorf("creating stub vmdk for %q: %w", vmCfg.Disk, writeErr)
			}

			// Link descriptor/backing files into each VM directory.
			// Supports both naming schemes:
			// - <base>.vmdk + <base>-flat.vmdk
			// - <base>-flat.vmdk + <base>-flat-flat.vmdk
			vmDir := filepath.Join(datastoreDir, vmName)
			if err := os.MkdirAll(vmDir, 0755); err != nil {
				return fmt.Errorf("creating vm datastore dir for %q: %w", vmName, err)
			}

			baseName := strings.TrimSuffix(vmCfg.Disk, ".vmdk")
			baseName = strings.TrimSuffix(baseName, "-flat")

			descriptorCandidates := uniqueNonEmpty(
				vmCfg.Disk,
				baseName+".vmdk",
				baseName+"-flat.vmdk",
			)
			selectedDescriptor := ""
			for _, cand := range descriptorCandidates {
				if fileExists(filepath.Join(cfg.DiskPath, cand)) {
					selectedDescriptor = cand
					break
				}
			}
			if selectedDescriptor != "" {
				descriptorPath := filepath.Join(vmDir, vmCfg.Disk)
				_ = os.Remove(descriptorPath)
				_ = os.Symlink(filepath.Join(cfg.DiskPath, selectedDescriptor), descriptorPath)
			}

			// libvirt's ESX driver (and real vSphere) find a flat disk's raw
			// extent data by naming convention: "<diskname>-flat.vmdk" *is*
			// the raw data, found directly rather than by parsing descriptor
			// text. Some disk preparation pipelines instead leave a small
			// text descriptor at that exact path, with the real data one
			// level further down (e.g. "<diskname>-flat-flat.vmdk"). Resolve
			// through that indirection so the conventional name always
			// points straight at the real data.
			flatName := baseName + "-flat.vmdk"
			if fileExists(filepath.Join(cfg.DiskPath, flatName)) {
				resolved := resolveFlatExtent(cfg.DiskPath, flatName)
				src := filepath.Join(cfg.DiskPath, resolved)
				if fileExists(src) {
					dst := filepath.Join(vmDir, flatName)
					_ = os.Remove(dst)
					_ = os.Symlink(src, dst)
				}
			}

			// Also expose the underlying extent file under its own original
			// name, in case some consumer looks it up directly.
			if extraExtent := baseName + "-flat-flat.vmdk"; fileExists(filepath.Join(cfg.DiskPath, extraExtent)) {
				src := filepath.Join(cfg.DiskPath, extraExtent)
				dst := filepath.Join(vmDir, extraExtent)
				_ = os.Remove(dst)
				_ = os.Symlink(src, dst)
			}

			vmDiskPath := fmt.Sprintf("[%s] %s", cfg.Datastore, vmCfg.Disk)

			networkName := cfg.Network
			if networkName == "" {
				networkName = "VM Network"
			}

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
					&types.VirtualDeviceConfigSpec{
						Operation: types.VirtualDeviceConfigSpecOperationAdd,
						Device: &types.VirtualVmxnet3{
							VirtualVmxnet: types.VirtualVmxnet{
								VirtualEthernetCard: types.VirtualEthernetCard{
									VirtualDevice: types.VirtualDevice{
										Key: -2,
										Backing: &types.VirtualEthernetCardNetworkBackingInfo{
											VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
												DeviceName: networkName,
											},
										},
									},
								},
							},
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
			vmxContent := generateVMX(vmName, guestID, vmCfg, networkName)
			if err := os.WriteFile(vmxPath, []byte(vmxContent), 0600); err != nil {
				log.Printf("Warning: could not write VMX for %s: %v", vmName, err)
			}

			log.Printf("Created VM: %s (guest=%s, disk=%s, %dGB)", vmName, guestID, vmCfg.Disk, vmCfg.DiskSizeGB)
		}
	}

	return nil
}

func generateVMX(vmName, guestID string, cfg VMConfig, networkName string) string {
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
ethernet0.networkName = "%s"
`, vmName, guestID, cfg.MemoryMB, cfg.NumCPUs, cfg.Firmware, scsiType, cfg.Disk, networkName)
}

func diskCapacityBytes(sizeGB int64) int64 {
	if sizeGB <= 0 {
		sizeGB = 10
	}
	return sizeGB * 1024 * 1024 * 1024
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// vmdkExtentRe matches a VMDK descriptor's extent description line, e.g.:
//
//	RW 9437184 FLAT "database-flat-flat.vmdk" 0
var vmdkExtentRe = regexp.MustCompile(`(?m)^\s*RW\s+\d+\s+FLAT\s+"([^"]+)"`)

// resolveFlatExtent follows a "<name>-flat.vmdk" candidate to the actual raw
// flat data file it should point to. libvirt's ESX driver (and real vSphere)
// expect "<diskname>-flat.vmdk" to *be* the raw extent data directly, found
// by naming convention rather than by parsing descriptor text — but some
// disk preparation pipelines (e.g. `qemu-img convert -O vmdk output-flat.vmdk`)
// produce a small text descriptor at that exact path, with the real data one
// level further down (e.g. "output-flat-flat.vmdk"). If path is itself a text
// descriptor, this returns the filename of the extent it references (resolved
// relative to dir); otherwise it returns name unchanged.
func resolveFlatExtent(dir, name string) string {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || info.Size() > 4096 {
		return name // too big to be a descriptor; assume it's real data
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "# Disk DescriptorFile") {
		return name
	}
	match := vmdkExtentRe.FindStringSubmatch(string(data))
	if len(match) < 2 {
		return name
	}
	return match[1]
}

func uniqueNonEmpty(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
