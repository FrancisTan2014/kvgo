package main

import (
	"context"
	"flag"
	"fmt"
	"kvgo/server"
	rafttransport "kvgo/transport/raft"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func parsePeers(raw string) ([]*rafttransport.PeerInfo, error) {
	if raw == "" {
		return nil, nil
	}
	var peers []*rafttransport.PeerInfo
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid peer %q: expected id=host:raftPort", entry)
		}
		id, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid peer ID %q: %w", parts[0], err)
		}
		raftAddr := parts[1]
		if raftAddr == "" {
			return nil, fmt.Errorf("invalid peer %q: address is empty", entry)
		}
		peers = append(peers, &rafttransport.PeerInfo{ID: id, Addr: raftAddr})
	}
	return peers, nil
}

func main() {
	var (
		nodeID       = flag.Uint64("node-id", 0, "unique node ID (required)")
		peersRaw     = flag.String("peers", "", "comma-separated peer list: id=host:raftPort,...")
		raftPort     = flag.Int("raft-port", 5000, "Raft transport listen port")
		network      = flag.String("network", "", "network type: tcp, tcp4, tcp6, unix (default tcp)")
		host         = flag.String("host", "", "listen address or unix socket path (default 127.0.0.1)")
		port         = flag.Int("port", 4000, "listen port (ignored for unix)")
		dataDir      = flag.String("data-dir", "", "data directory (required)")
		readTO       = flag.Duration("read-timeout", 0, "per-request read timeout (default 5s)")
		writeTO      = flag.Duration("write-timeout", 0, "per-request write timeout (default 5s)")
		maxFrame     = flag.Int("max-frame", 0, "max frame size in bytes (0 = default 16MB)")
		syncInterval = flag.Duration("sync", 0, "WAL fsync interval (0 = default 10ms, lower = faster but more fsync)")
		debug        = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	if *nodeID == 0 {
		fmt.Fprintln(os.Stderr, "error: -node-id is required")
		flag.Usage()
		os.Exit(2)
	}

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

	if *raftPort < 0 || *raftPort > 65535 {
		fmt.Fprintln(os.Stderr, "error: -raft-port must be in range 0-65535")
		os.Exit(2)
	}

	peers, err := parsePeers(*peersRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	var logger *slog.Logger
	if *debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}

	opts := server.Options{
		ID:           *nodeID,
		Peers:        peers,
		Network:      *network,
		Host:         *host,
		Port:         uint16(*port),
		RaftPort:     uint16(*raftPort),
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

	fmt.Printf("kv-server listening on %s\n", s.Addr())

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
