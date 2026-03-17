package nfc

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

const diskPrefix = "/disk/"

// DiskServer serves VMDK files over HTTP and handles ExportVm/HttpNfcLease
// methods for VM migration.
type DiskServer struct {
	DiskPath     string
	ExternalHost string
	ServerURL    string // set after server starts (e.g. "http://host:port")

	mu     sync.Mutex
	leases map[string]*lease
}

type lease struct {
	mo.HttpNfcLease
	files map[string]string // filename -> disk path on filesystem
	ds    *DiskServer
}

func (l *lease) Reference() types.ManagedObjectReference {
	return l.Self
}

func (l *lease) HttpNfcLeaseComplete(ctx *simulator.Context, req *types.HttpNfcLeaseComplete) soap.HasFault {
	ctx.Session.Remove(ctx, req.This)
	l.ds.mu.Lock()
	delete(l.ds.leases, req.This.Value)
	l.ds.mu.Unlock()

	return &methods.HttpNfcLeaseCompleteBody{
		Res: new(types.HttpNfcLeaseCompleteResponse),
	}
}

func (l *lease) HttpNfcLeaseAbort(ctx *simulator.Context, req *types.HttpNfcLeaseAbort) soap.HasFault {
	ctx.Session.Remove(ctx, req.This)
	l.ds.mu.Lock()
	delete(l.ds.leases, req.This.Value)
	l.ds.mu.Unlock()

	return &methods.HttpNfcLeaseAbortBody{
		Res: new(types.HttpNfcLeaseAbortResponse),
	}
}

func (l *lease) HttpNfcLeaseProgress(ctx *simulator.Context, req *types.HttpNfcLeaseProgress) soap.HasFault {
	l.TransferProgress = req.Percent

	return &methods.HttpNfcLeaseProgressBody{
		Res: new(types.HttpNfcLeaseProgressResponse),
	}
}

func (l *lease) HttpNfcLeaseGetManifest(ctx *simulator.Context, req *types.HttpNfcLeaseGetManifest) soap.HasFault {
	return &methods.HttpNfcLeaseGetManifestBody{
		Res: &types.HttpNfcLeaseGetManifestResponse{},
	}
}

func NewDiskServer(diskPath, externalHost string) *DiskServer {
	return &DiskServer{
		DiskPath:     diskPath,
		ExternalHost: externalHost,
		leases:       make(map[string]*lease),
	}
}

// FVMWVirtualMachine wraps simulator.VirtualMachine to add ExportVm support.
// It is registered in the vcsim registry to replace the default VM objects.
type FVMWVirtualMachine struct {
	*simulator.VirtualMachine
	ds *DiskServer
}

// RegisterObject interface — required for AddHandler. These are event callbacks
// that we don't need to act on.
func (vm *FVMWVirtualMachine) PutObject(_ *simulator.Context, _ mo.Reference)    {}
func (vm *FVMWVirtualMachine) UpdateObject(_ *simulator.Context, _ mo.Reference, _ []types.PropertyChange) {
}
func (vm *FVMWVirtualMachine) RemoveObject(_ *simulator.Context, _ types.ManagedObjectReference) {}

// ExportVm implements the vSphere ExportVm API method.
// It creates an HttpNfcLease with disk download URLs for the VM's disks.
func (vm *FVMWVirtualMachine) ExportVm(ctx *simulator.Context, req *types.ExportVm) soap.HasFault {
	l := &lease{
		HttpNfcLease: mo.HttpNfcLease{
			State:              types.HttpNfcLeaseStateReady,
			InitializeProgress: 100,
			TransferProgress:   0,
			Mode:               string(types.HttpNfcLeaseModePushOrGet),
			Capabilities: types.HttpNfcLeaseCapabilities{
				CorsSupported:     true,
				PullModeSupported: true,
			},
		},
		files: make(map[string]string),
		ds:    vm.ds,
	}

	// Register in session so PropertyCollector can find it
	ctx.Session.Put(l)

	// Build device URLs from VM's disk backing
	device := object.VirtualDeviceList(vm.Config.Hardware.Device)
	ndevice := make(map[string]int)
	var urls []types.HttpNfcLeaseDeviceUrl

	for _, d := range device {
		info, ok := d.GetVirtualDevice().Backing.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			continue
		}
		var file object.DatastorePath
		file.FromString(info.GetVirtualDeviceFileBackingInfo().FileName)
		name := path.Base(file.Path)

		// Resolve to actual file on disk
		diskFile := filepath.Join(vm.ds.DiskPath, name)
		l.files[name] = diskFile

		_, isDisk := d.(*types.VirtualDisk)
		kind := device.Type(d)
		n := ndevice[kind]
		ndevice[kind]++

		u := vm.ds.buildURL(l.Self.Value, name)

		urls = append(urls, types.HttpNfcLeaseDeviceUrl{
			Key:       fmt.Sprintf("/%s/%s:%d", vm.Self.Value, kind, n),
			ImportKey: fmt.Sprintf("/%s/%s:%d", vm.Name, kind, n),
			Url:       u,
			Disk:      types.NewBool(isDisk),
			TargetId:  name,
		})
	}

	l.Info = &types.HttpNfcLeaseInfo{
		Lease:        l.Self,
		Entity:       vm.Self,
		DeviceUrl:    urls,
		LeaseTimeout: 300,
	}

	vm.ds.mu.Lock()
	vm.ds.leases[l.Self.Value] = l
	vm.ds.mu.Unlock()

	log.Printf("ExportVm: created lease %s for VM %s with %d device URLs",
		l.Self.Value, vm.Name, len(urls))

	return &methods.ExportVmBody{
		Res: &types.ExportVmResponse{
			Returnval: l.Self,
		},
	}
}

// WrapVMs registers FVMWVirtualMachine handlers for all VMs in the registry.
// Uses AddHandler so the original *simulator.VirtualMachine stays in the map
// for internal vcsim operations (snapshots, etc.), while our wrapper intercepts
// ExportVm calls via the handler dispatch.
func (ds *DiskServer) WrapVMs(ctx *simulator.Context) {
	for _, entity := range ctx.Map.All("VirtualMachine") {
		vm := ctx.Map.Get(entity.Reference()).(*simulator.VirtualMachine)
		wrapped := &FVMWVirtualMachine{
			VirtualMachine: vm,
			ds:             ds,
		}
		ctx.Map.AddHandler(wrapped)
	}
}

func (ds *DiskServer) buildURL(leaseID, filename string) string {
	diskPath := diskPrefix + leaseID + "/" + filename
	if ds.ExternalHost != "" {
		u := &url.URL{
			Scheme: "https",
			Host:   ds.ExternalHost,
			Path:   diskPath,
		}
		return u.String()
	}
	if ds.ServerURL != "" {
		return ds.ServerURL + diskPath
	}
	return diskPath
}

// ServeHTTP handles disk file downloads at /disk/<leaseID>/<filename>.
func (ds *DiskServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, diskPrefix), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	leaseID, filename := parts[0], parts[1]

	ds.mu.Lock()
	l, ok := ds.leases[leaseID]
	ds.mu.Unlock()
	if !ok {
		log.Printf("disk: unknown lease %s", leaseID)
		http.NotFound(w, r)
		return
	}

	filePath, ok := l.files[filename]
	if !ok {
		log.Printf("disk: unknown file %s in lease %s", filename, leaseID)
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f, err := os.Open(filePath)
		if err != nil {
			log.Printf("disk: failed to open %s: %v", filePath, err)
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size()))

		n, err := io.Copy(w, f)
		if err != nil {
			log.Printf("disk: error streaming %s: %v", filePath, err)
			return
		}
		log.Printf("disk: served %s (%d bytes)", filePath, n)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
