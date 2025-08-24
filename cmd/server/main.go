package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sip-server/internal/sip"
	"sip-server/internal/storage"
	"sip-server/internal/web"
	"syscall"
	"golang.org/x/sync/errgroup"
)

func main() {
	// --- Configuration ---
	var (
		webAddr = flag.String("web.addr", ":8080", "Address for the web UI server")
		sipAddr = flag.String("sip.addr", ":5060", "Address for the SIP server")
		dbPath  = flag.String("db.path", "sip_users.db", "Path to the SQLite database file")
		realm   = flag.String("sip.realm", "go-sip-server", "SIP realm for authentication")
	)
	flag.Parse()

	// --- Application Setup ---
	log.Println("Initializing application...")

	// Initialize storage
	s, err := storage.NewStorage(*dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer s.Close()
	log.Printf("Storage initialized with database file: %s", *dbPath)

	// Create SIP server
	sipServer := sip.NewSIPServer(s, *realm)

	// Create web server
	webServer, err := web.NewServer(s, *realm, sipServer)
	if err != nil {
		log.Fatalf("Failed to create web server: %v", err)
	}

	// --- Server Execution ---
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gCtx := errgroup.WithContext(ctx)

	// Start web server
	g.Go(func() error {
		log.Printf("Web server starting on %s", *webAddr)
		if err := webServer.Run(*webAddr); err != nil {
			log.Printf("Web server failed: %v", err)
			return err
		}
		return nil
	})

	// Start SIP server
	g.Go(func() error {
		log.Printf("SIP server starting on %s", *sipAddr)
		if err := sipServer.Run(gCtx, *sipAddr); err != nil {
			log.Printf("SIP server failed: %v", err)
			return err
		}
		return nil
	})

	log.Println("Application started. Press Ctrl+C to exit.")

	// Wait for shutdown signal or for a server to fail
	if err := g.Wait(); err != nil {
		log.Printf("Application exited with error: %v", err)
	} else {
		log.Println("Application shutting down gracefully.")
	}
}
