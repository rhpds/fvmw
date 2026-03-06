package main

import (
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

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

	// Set the listen address for the vcsim server
	model.Service.Listen = &url.URL{Host: cfg.ListenAddr}

	// Register disk file serving handler on the service mux
	model.Service.HandleFunc("/disk/", diskServer.ServeHTTP)

	// Start the vcsim HTTP server
	server := model.Service.NewServer()
	defer server.Close()

	// Set the server URL for disk download URLs (used when no external host is configured)
	serverBase := &url.URL{Scheme: server.URL.Scheme, Host: server.URL.Host}
	diskServer.ServerURL = serverBase.String()

	log.Printf("vcsim server started on %s", server.URL)
	log.Printf("SDK endpoint: %s/sdk", server.URL)

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down...")
}
