package main

import (
	"context"
	"flag"
	"fmt"
	"kvgo/server"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		network      = flag.String("network", "", "network type: tcp, tcp4, tcp6, unix (default tcp)")
		host         = flag.String("host", "", "listen address or unix socket path (default 127.0.0.1)")
		port         = flag.Int("port", 4000, "listen port (ignored for unix)")
		replicaOf    = flag.String("replica-of", "", "primary address to replicate from (enables replica mode)")
		dataDir      = flag.String("data-dir", "", "data directory (required)")
		readTO       = flag.Duration("read-timeout", 0, "per-request read timeout (0 = no timeout)")
		writeTO      = flag.Duration("write-timeout", 0, "per-request write timeout (0 = no timeout)")
		maxFrame     = flag.Int("max-frame", 0, "max frame size in bytes (0 = default 16MB)")
		syncInterval = flag.Duration("sync", 0, "WAL fsync interval (0 = default 10ms, lower = faster but more fsync)")
		debug        = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "error: -data-dir is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create data directory: %v\n", err)
		os.Exit(1)
	}

	if *port < 0 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "error: -port must be in range 0-65535")
		os.Exit(2)
	}

	var logger *log.Logger
	if *debug {
		logger = log.New(os.Stderr, "[kv-server] ", log.LstdFlags|log.Lmicroseconds)
	}

	opts := server.Options{
		Network:      *network,
		Host:         *host,
		Port:         uint16(*port),
		ReplicaOf:    *replicaOf,
		DataDir:      *dataDir,
		ReadTimeout:  *readTO,
		WriteTimeout: *writeTO,
		MaxFrameSize: *maxFrame,
		SyncInterval: *syncInterval,
		Logger:       logger,
	}

	s, err := server.NewServer(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := s.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *replicaOf != "" {
		fmt.Printf("kv-server (replica of %s) listening on %s\n", *replicaOf, s.Addr())
	} else {
		fmt.Printf("kv-server (primary) listening on %s\n", s.Addr())
	}

	// Wait for interrupt signal.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	fmt.Println("shutting down (5s grace period)...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("bye")
}
