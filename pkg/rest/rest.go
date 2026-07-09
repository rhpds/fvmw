package rest

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// Handler implements the vSphere REST API subset needed by os-migrate.
// Endpoints:
//
//	POST /api/session
//	GET  /api/vcenter/vm
//	GET  /api/vcenter/vm/{vm}/hardware/disk
//	GET  /api/vcenter/vm/{vm}/hardware/disk/{disk}
type Handler struct {
	ctx            *simulator.Context
	credentials    map[string]string
	scopeVMsByUser bool

	mu       sync.Mutex
	sessions map[string]string // token -> username
}

func NewHandler(ctx *simulator.Context, credentials map[string]string, scopeVMsByUser bool) *Handler {
	credCopy := make(map[string]string, len(credentials))
	for user, pass := range credentials {
		credCopy[user] = pass
	}

	return &Handler{
		ctx:            ctx,
		credentials:    credCopy,
		scopeVMsByUser: scopeVMsByUser,
		sessions:       make(map[string]string),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")

	if path == "/api/session" {
		h.handleSession(w, r)
		return
	}

	// Accept session token (header/cookie) and basic auth for compatibility.
	user, valid := h.authenticate(r)
	if !valid {
		http.Error(w, `{"error_type":"UNAUTHENTICATED"}`, http.StatusUnauthorized)
		return
	}

	switch {
	case path == "/api":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "ok"})
	case path == "/api/vcenter/vm":
		h.handleListVMs(w, r, user)
	case strings.HasPrefix(path, "/api/vcenter/vm/"):
		h.handleVMSubresource(w, r, path, user)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		token := r.Header.Get("vmware-api-session-id")
		h.mu.Lock()
		delete(h.sessions, token)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user, pass, ok := r.BasicAuth()
	if !ok || !h.validCredentials(user, pass) {
		http.Error(w, `{"error_type":"UNAUTHENTICATED"}`, http.StatusUnauthorized)
		return
	}

	token := "fvmw-session-" + uuid.NewString()
	h.mu.Lock()
	h.sessions[token] = user
	h.mu.Unlock()

	// Return token in body and headers/cookie for broad client compatibility.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("vmware-api-session-id", token)
	http.SetCookie(w, &http.Cookie{
		Name:     "vmware-api-session-id",
		Value:    token,
		Path:     "/api",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	_ = json.NewEncoder(w).Encode(token)
}

func (h *Handler) validCredentials(user, pass string) bool {
	expectedPass, ok := h.credentials[user]
	return ok && pass == expectedPass
}

func (h *Handler) authenticate(r *http.Request) (string, bool) {
	// Preferred: vmware-api-session-id header/cookie.
	token := strings.TrimSpace(r.Header.Get("vmware-api-session-id"))
	if token == "" {
		if c, err := r.Cookie("vmware-api-session-id"); err == nil {
			token = strings.TrimSpace(c.Value)
		}
	}
	if token != "" {
		// Some clients may copy JSON string payload including quotes.
		token = strings.Trim(token, "\"")
		h.mu.Lock()
		user, valid := h.sessions[token]
		h.mu.Unlock()
		if valid {
			return user, true
		}
	}

	// Fallback: basic auth.
	user, pass, ok := r.BasicAuth()
	if ok && h.validCredentials(user, pass) {
		return user, true
	}
	return "", false
}

type vmSummary struct {
	MemorySizeMiB int32  `json:"memory_size_MiB"`
	VM            string `json:"vm"`
	Name          string `json:"name"`
	PowerState    string `json:"power_state"`
	CPUCount      int32  `json:"cpu_count"`
}

func (h *Handler) handleListVMs(w http.ResponseWriter, r *http.Request, user string) {
	nameFilter := r.URL.Query().Get("names")
	var results []vmSummary

	for _, entity := range h.ctx.Map.All("VirtualMachine") {
		vm := h.ctx.Map.Get(entity.Reference())
		if vm == nil {
			continue
		}

		var props struct {
			Name    string
			Config  *types.VirtualMachineConfigInfo
			Runtime types.VirtualMachineRuntimeInfo
		}

		simVM := getSimVM(vm)
		if simVM == nil {
			continue
		}

		props.Name = simVM.Name
		props.Config = simVM.Config
		props.Runtime = simVM.Runtime

		if h.scopeVMsByUser {
			userSuffix := "-" + user
			if !strings.HasSuffix(props.Name, userSuffix) {
				continue
			}
		}

		if nameFilter != "" && props.Name != nameFilter {
			continue
		}

		powerState := "POWERED_OFF"
		if props.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
			powerState = "POWERED_ON"
		}

		var memMB int32
		var cpus int32
		if props.Config != nil {
			memMB = props.Config.Hardware.MemoryMB
			cpus = props.Config.Hardware.NumCPU
		}

		results = append(results, vmSummary{
			MemorySizeMiB: memMB,
			VM:            entity.Reference().Value,
			Name:          props.Name,
			PowerState:    powerState,
			CPUCount:      cpus,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}

func (h *Handler) handleVMSubresource(w http.ResponseWriter, r *http.Request, path string, user string) {
	// Parse: /api/vcenter/vm/{vm-id}/hardware/disk[/{disk-id}]
	trimmed := strings.TrimPrefix(path, "/api/vcenter/vm/")
	parts := strings.SplitN(trimmed, "/", 4) // vm-id[/hardware/disk[/disk-id]]

	vmID := parts[0]
	ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmID}
	obj := h.ctx.Map.Get(ref)
	if obj == nil {
		http.Error(w, `{"error_type":"NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	simVM := getSimVM(obj)
	if simVM == nil {
		http.Error(w, `{"error_type":"NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	if h.scopeVMsByUser {
		userSuffix := "-" + user
		if !strings.HasSuffix(simVM.Name, userSuffix) {
			http.Error(w, `{"error_type":"NOT_FOUND"}`, http.StatusNotFound)
			return
		}
	}
	config := simVM.Config

	if config == nil {
		http.Error(w, `{"error_type":"NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	// Must be /hardware/disk or /hardware/disk/{id}
	if len(parts) < 3 || parts[1] != "hardware" || parts[2] != "disk" {
		http.NotFound(w, r)
		return
	}

	devices := object.VirtualDeviceList(config.Hardware.Device)

	if len(parts) == 3 {
		// List disks: GET /api/vcenter/vm/{vm}/hardware/disk
		var diskList []map[string]string
		for _, d := range devices {
			if _, ok := d.(*types.VirtualDisk); ok {
				diskList = append(diskList, map[string]string{
					"disk": fmt.Sprintf("%d", d.GetVirtualDevice().Key),
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diskList)
		return
	}

	// Get specific disk: GET /api/vcenter/vm/{vm}/hardware/disk/{disk-key}
	diskKey := parts[3]
	for _, d := range devices {
		disk, ok := d.(*types.VirtualDisk)
		if !ok {
			continue
		}
		if fmt.Sprintf("%d", disk.Key) != diskKey {
			continue
		}

		vdev := disk.GetVirtualDevice()
		resp := map[string]interface{}{
			"label":    devices.Name(d),
			"type":     "SCSI",
			"capacity": disk.CapacityInBytes,
		}

		// SCSI address
		if vdev.UnitNumber != nil {
			busNumber := int32(0)
			if vdev.ControllerKey != 0 {
				for _, ctrl := range devices {
					if ctrl.GetVirtualDevice().Key == vdev.ControllerKey {
						if sc, ok := ctrl.(types.BaseVirtualSCSIController); ok {
							busNumber = sc.GetVirtualSCSIController().BusNumber
						}
					}
				}
			}
			resp["scsi"] = map[string]int32{
				"bus":  busNumber,
				"unit": *vdev.UnitNumber,
			}
		}

		// Backing
		if backing, ok := vdev.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
			resp["backing"] = map[string]string{
				"vmdk_file": backing.FileName,
				"type":      "VMDK_FILE",
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	http.Error(w, `{"error_type":"NOT_FOUND"}`, http.StatusNotFound)
}

// getSimVM extracts the simulator.VirtualMachine from a registry object.
func getSimVM(obj interface{}) *simulator.VirtualMachine {
	if v, ok := obj.(*simulator.VirtualMachine); ok {
		return v
	}
	return nil
}

// Register adds the REST API handler to the given mux.
func Register(mux *http.ServeMux, ctx *simulator.Context, credentials map[string]string, scopeVMsByUser bool) {
	handler := NewHandler(ctx, credentials, scopeVMsByUser)
	mux.Handle("/api/", handler)
	log.Println("Registered vSphere REST API handler at /api/")
}
