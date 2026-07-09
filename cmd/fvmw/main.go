package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rhpds/fvmw/pkg/fixup"
	"github.com/rhpds/fvmw/pkg/inventory"
	"github.com/rhpds/fvmw/pkg/nfc"
	"github.com/rhpds/fvmw/pkg/rest"
	"github.com/rhpds/fvmw/pkg/ui"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

func main() {
	configPath := flag.String("config", "config/default.yaml", "path to inventory config file")
	flag.Parse()

	cfg, err := inventory.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Starting fvmw with config: datacenter=%s cluster=%s host=%s",
		cfg.Datacenter, cfg.Cluster, cfg.Host)
	log.Printf("Listen: %s, DiskPath: %s, ExternalHost: %s",
		cfg.ListenAddr, cfg.DiskPath, cfg.ExternalHost)
	log.Printf("Configured users: %v", sortedUsers(cfg.AuthCredentials()))

	// Enable SOAP trace logging if requested
	if os.Getenv("FVMW_TRACE") != "" {
		simulator.Trace = true
		log.Println("SOAP trace logging enabled")
	}

	// Build the vcsim model from config
	model, err := inventory.Build(cfg)
	if err != nil {
		log.Fatalf("Failed to build inventory: %v", err)
	}
	defer model.Remove()

	credentials := cfg.AuthCredentials()
	scopeVMsByUser := cfg.ScopeVMsByUser()

	// Force simulator to use SessionManager.ValidLogin (instead of the default
	// "any non-empty user/password" behavior) so we can support a real user map.
	model.Service.Listen = &url.URL{User: url.UserPassword("fvmw-auth", "fvmw-auth")}
	model.Service.Context.Map.SessionManager().ValidLogin = func(req *types.Login) bool {
		if req.UserName == "" || req.Password == "" {
			return false
		}
		expectedPass, ok := credentials[req.UserName]
		return ok && req.Password == expectedPass
	}

	// Create the disk server for ExportVm / HttpNfcLease
	diskServer := nfc.NewDiskServer(cfg.DiskPath, cfg.ExternalHost)

	// Replace VM objects with wrapped versions that support ExportVm
	diskServer.WrapVMs(model.Service.Context)

	// Register disk file serving handler on the service mux
	model.Service.HandleFunc("/disk/", diskServer.ServeHTTP)

	// Let vcsim set up all its internal handlers on a random port.
	// We won't expose this port — we'll proxy through our own listener.
	server := model.Service.NewServer()
	defer server.Close()

	// Register vSphere REST API handler (for os-migrate vmware.vmware_rest)
	// Must be after NewServer() which initializes ServeMux.
	rest.Register(model.Service.ServeMux, model.Service.Context, credentials, scopeVMsByUser)
	ui.Register(model.Service.ServeMux)

	// Set the server URL for disk download URLs
	if cfg.ExternalHost != "" {
		diskServer.ServerURL = ""
	} else {
		serverBase := &url.URL{Scheme: "http", Host: cfg.ListenAddr}
		diskServer.ServerURL = serverBase.String()
	}

	// Start our own HTTP listener that wraps vcsim's mux with the
	// XML namespace rewriter. govmomi's encoder uses _XMLSchema-instance:
	// instead of xsi: which breaks libvirt's ESX driver (used by virt-v2v).
	allowedVMRefs := func(user string) map[string]struct{} {
		allowed := make(map[string]struct{})
		if user == "" {
			return allowed
		}
		userSuffix := "-" + user
		for _, entity := range model.Service.Context.Map.All("VirtualMachine") {
			obj := model.Service.Context.Map.Get(entity.Reference())
			if obj == nil {
				continue
			}
			vm, ok := obj.(*simulator.VirtualMachine)
			if !ok {
				continue
			}
			name := vm.Name
			if strings.HasSuffix(name, userSuffix) {
				allowed[entity.Reference().Value] = struct{}{}
			}
		}
		return allowed
	}

	rewriter := &fixup.XMLRewriter{
		Handler:        model.Service.ServeMux,
		ESXiPrefix:     cfg.ESXiPrefix,
		ScopeVMsByUser: scopeVMsByUser,
		AllowedVMRefs:  allowedVMRefs,
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", cfg.ListenAddr, err)
	}

	httpServer := &http.Server{
		Handler:           rewriter,
		ReadHeaderTimeout: 60 * time.Second,
	}

	go func() {
		log.Printf("Listening on %s", cfg.ListenAddr)
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Println("fvmw ready")

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down...")
}

func sortedUsers(credentials map[string]string) []string {
	users := make([]string, 0, len(credentials))
	for user := range credentials {
		users = append(users, user)
	}
	sort.Strings(users)
	return users
}
