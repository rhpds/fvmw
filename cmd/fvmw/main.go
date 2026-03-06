package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
	"os/signal"
	"syscall"

	"github.com/rhpds/fvmw/pkg/fixup"
	"github.com/rhpds/fvmw/pkg/inventory"
	"github.com/rhpds/fvmw/pkg/nfc"
	"github.com/vmware/govmomi/simulator"
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

	// Enable SOAP trace logging if requested
	if os.Getenv("FVMW_TRACE") != "" {
		simulator.Trace = true
		log.Println("SOAP trace logging enabled")
	}

	// Configure authentication before building the model
	simulator.DefaultLogin = url.UserPassword(cfg.Username, cfg.Password)

	// Build the vcsim model from config
	model, err := inventory.Build(cfg)
	if err != nil {
		log.Fatalf("Failed to build inventory: %v", err)
	}
	defer model.Remove()

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
	rewriter := &fixup.XMLRewriter{
		Handler:    model.Service.ServeMux,
		ESXiPrefix: "esxi-",
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
