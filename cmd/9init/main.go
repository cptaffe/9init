// 9init is a 9P-native session init system for plan9port environments.
//
// Usage:
//
//	9init [-services dir] [-logdir dir] [subcommand args...]
//
// When run without a subcommand it starts the daemon. Subcommands
// delegate to the companion ctl client (see cmd/9init/ctl).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"9fans.net/go/plan9/client"
	"github.com/cptaffe/9init/internal/config"
	"github.com/cptaffe/9init/internal/fs9p"
	"github.com/cptaffe/9init/internal/graph"
	"github.com/cptaffe/9init/internal/supervisor"
	"github.com/cptaffe/9init/internal/watcher"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("9init: home dir: %v", err)
	}

	svcDir := flag.String("services", filepath.Join(home, "lib/9init"), "directory of *.rc service scripts")
	logDir := flag.String("logdir", filepath.Join(home, "Library/Logs/9init"), "directory for rotating log files")
	flag.Parse()

	log.SetPrefix("9init: ")
	log.SetFlags(log.Ldate | log.Ltime)

	// Load and validate service definitions.
	services, err := config.LoadDir(*svcDir)
	if err != nil {
		log.Fatalf("load services: %v", err)
	}
	if len(services) == 0 {
		log.Fatalf("no *.rc files found in %s", *svcDir)
	}

	// Build and validate the dependency graph.
	g, err := graph.Build(services)
	if err != nil {
		log.Fatalf("dependency graph: %v", err)
	}

	// Resolve the plan9port namespace directory.
	ns := client.Namespace()
	if ns == "" {
		log.Fatalf("cannot determine namespace directory (is plan9port installed?)")
	}
	log.Printf("namespace: %s", ns)

	// Create the namespace watcher (kqueue on Darwin).
	w, err := watcher.New(ns)
	if err != nil {
		log.Fatalf("watcher: %v", err)
	}
	defer w.Close()

	// Create the supervisor.
	sup, err := supervisor.New(g, ns, *logDir)
	if err != nil {
		log.Fatalf("supervisor: %v", err)
	}

	// Post the 9P filesystem at $NAMESPACE/init.
	srvPath := filepath.Join(ns, "init")
	fsServer, rw, fsCleanup, err := fs9p.Listen(srvPath, sup)
	if err != nil {
		log.Fatalf("fs9p listen %s: %v", srvPath, err)
	}
	defer fsCleanup()
	log.Printf("serving 9P at %s", srvPath)

	// Serve the 9P tree in the background.
	go fsServer.Serve(rw)

	// Handle SIGTERM and SIGINT for clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// Run the supervisor event loop (blocks until ctx is done or shutdown command).
	if err := sup.Run(ctx, w); err != nil && err != context.Canceled {
		log.Printf("supervisor exited: %v", err)
	}

	log.Printf("shutdown complete")
}
