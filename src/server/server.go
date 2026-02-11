package server

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"kvgo/engine"
	"kvgo/protocol"
	"kvgo/utils"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultHost    = "127.0.0.1" // localhost only; override with Options.Host
	defaultNetwork = NetworkTCP
)

// Consistency Model
//
// Replication is async, fire-and-forget with no delivery guarantees.
// The primary forwards writes to replicas but does not wait for acknowledgment.
// If a replica disconnects and reconnects, it will miss all writes during the gap.
//
// This is eventual consistency at best — replicas may lag or diverge permanently.
// We accept this trade-off to keep the system simple and fast (following Redis).
//
// Sequence numbers provide visibility into replication lag but do not fix it.

// Supported network types for Options.Network.
const (
	NetworkTCP  = "tcp"
	NetworkTCP4 = "tcp4"
	NetworkTCP6 = "tcp6"
	NetworkUnix = "unix"
)

var supportedNetworks = map[string]bool{
	NetworkTCP:  true,
	NetworkTCP4: true,
	NetworkTCP6: true,
	NetworkUnix: true,
}

var (
	ErrAlreadyStarted = errors.New("server: already started")
	ErrNotStarted     = errors.New("server: not started")
	ErrDBInUse        = errors.New("server: database is already in use")

	noopLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type Options struct {
	Network   string // NetworkTCP, NetworkTCP4, NetworkTCP6, or NetworkUnix
	Host      string // address, or socket path when Network is NetworkUnix
	Port      uint16 // ignored when Network is NetworkUnix
	ReplicaOf string // non-empty value indicates that it is a follower
	DataDir   string

	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	MaxFrameSize        int
	BacklogSizeLimit    int64         // default 16MB
	BacklogTrimDuration time.Duration // default 100ms

	StrongReadTimeout  time.Duration // default 100ms, how long to wait for replica to catch up
	QuorumWriteTimeout time.Duration // default 500ms, how long to wait for quorum ACKs before timeout (Episode 026)

	// SyncInterval controls how often the WAL is fsynced (latency vs throughput tradeoff).
	// Lower values reduce latency but increase fsync overhead.
	// Zero means use engine.DefaultSyncInterval (100ms).
	SyncInterval time.Duration

	// Staleness bounds (Episode 025): replica rejects reads when exceeding thresholds
	ReplicaStaleHeartbeat time.Duration // time-based: reject if no heartbeat for N seconds (partition detection), default 5s
	ReplicaStaleLag       int           // sequence-based: reject if > N operations behind (backlog limit), default 1000

	Logger *slog.Logger // optional debug logger; nil disables logging
}

type quorumState struct {
	mu            sync.Mutex // Per-request lock
	needed        int
	ackCount      int32 // Protected by mu
	ackCh         chan struct{}
	closeOnce     sync.Once
	failed        atomic.Bool           // true if NACK received
	ackedReplicas map[net.Conn]struct{} // Track which replicas ACK'd (protected by mu)
}

type Server struct {
	opts    Options
	started atomic.Bool
	ctx     context.Context // Server lifetime
	cancel  context.CancelFunc

	// Core infrastructure
	ln   net.Listener
	db   *engine.DB
	lock *dataDirLock

	// Connection management
	mu    sync.Mutex
	wg    sync.WaitGroup
	conns map[net.Conn]struct{}

	// Request dispatch
	requestHandlers map[protocol.Cmd]HandlerFunc

	// Replication state (primary role)
	seq      atomic.Uint64 // monotonic sequence number for writes
	replicas map[net.Conn]*replicaConn
	replid   string

	// Quorum control
	quorumMu     sync.RWMutex
	quorumWrites map[string]*quorumState
	quorumAckCh  chan string // Quorum ack channel

	// Replication state (replica role)
	isReplica     bool
	primary       net.Conn
	lastSeq       atomic.Uint64 // last applied sequence number
	lastHeartbeat time.Time     // updated from heartbeat messages
	primarySeq    uint64        // primary's position from heartbeat; compared with lastSeq for staleness detection

	// Replication loop control
	replCtx    context.Context
	replCancel context.CancelFunc

	// Partial resync backlog
	backlog       list.List
	backlogSize   atomic.Int64 // Int64 for direct subtraction; always non-negative
	backlogMu     sync.RWMutex
	backlogCtx    context.Context
	backlogCancel context.CancelFunc

	// Cleanup
	cleanupInProgress atomic.Bool

	// Persistence
	metaFile *os.File
}

func NewServer(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("server: DataDir is required")
	}
	if opts.Network != "" && !supportedNetworks[opts.Network] {
		return nil, fmt.Errorf("server: unsupported Network %q", opts.Network)
	}
	if opts.Network == NetworkUnix && opts.Host == "" {
		return nil, fmt.Errorf("server: Host (socket path) is required for %s network", NetworkUnix)
	}
	if opts.MaxFrameSize <= 0 {
		// Default to protocol DefaultMaxFrameSize, but keep it local to avoid
		// pulling protocol into the server config.
		opts.MaxFrameSize = 16 << 20
	}
	if opts.BacklogSizeLimit <= 0 {
		opts.BacklogSizeLimit = 16 << 20 // 16MB
	}
	if opts.BacklogTrimDuration <= 0 {
		opts.BacklogTrimDuration = 100 * time.Millisecond
	}
	if opts.StrongReadTimeout <= 0 {
		opts.StrongReadTimeout = 100 * time.Millisecond
	}
	if opts.QuorumWriteTimeout <= 0 {
		opts.QuorumWriteTimeout = 500 * time.Millisecond
	}
	if opts.ReplicaStaleHeartbeat <= 0 {
		opts.ReplicaStaleHeartbeat = 5 * time.Second
	}
	if opts.ReplicaStaleLag <= 0 {
		opts.ReplicaStaleLag = 1000
	}

	isReplica := opts.ReplicaOf != ""
	replicas := make(map[net.Conn]*replicaConn)

	s := &Server{
		opts:            opts,
		conns:           make(map[net.Conn]struct{}),
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
		isReplica:       isReplica,
		replicas:        replicas,
		lastHeartbeat:   time.Now(), // Initialize heartbeat timer (updated by PING messages)
		quorumAckCh:     make(chan string),
		quorumWrites:    make(map[string]*quorumState),
	}

	s.registerRequestHandlers()
	return s, nil
}

func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Start() (err error) {
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}

	var (
		lock *dataDirLock
		db   *engine.DB
		ln   net.Listener
	)

	defer func() {
		if err != nil {
			// Cancel replication loop if started
			if s.replCancel != nil {
				s.replCancel()
			}
			if ln != nil {
				_ = ln.Close()
			}
			if db != nil {
				_ = db.Close()
			}
			if lock != nil {
				_ = lock.Close()
			}
			s.started.Store(false)
		}
	}()

	if err = s.restoreState(); err != nil {
		return err
	}

	if s.replid == "" {
		s.replid = utils.GenerateUniqueID()
	}

	lock, err = acquireDataDirLock(s.opts.DataDir)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return ErrDBInUse
		}
		return err
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())
	db, err = engine.NewDBWithOptions(s.opts.DataDir, engine.Options{
		SyncInterval: s.opts.SyncInterval,
		Logger:       s.opts.Logger,
	}, s.ctx)
	if err != nil {
		return err
	}
	s.db = db // Assign now so startReplicationLoop can use it

	// Start replication loop only for replicas
	if s.isReplica {
		s.startReplicationLoop()
	} else {
		s.startBacklogTrimmer()
	}

	ln, err = net.Listen(s.network(), s.listenAddr())
	if err != nil {
		return err
	}

	s.lock, s.ln = lock, ln
	go s.acceptLoop()
	go s.monitorDBHealth()
	return nil
}

func (s *Server) network() string {
	if s.opts.Network == "" {
		return defaultNetwork
	}
	return s.opts.Network
}

func (s *Server) monitorDBHealth() {
	<-s.db.FatalErr // blocks until critical error occurs

	s.log().Error("FATAL: database encountered critical error, initiating shutdown",
		"data_dir", s.opts.DataDir,
		"action", "graceful_shutdown",
		"recovery", "process_will_restart_and_replay_wal",
	)

	// Initiate graceful shutdown (with timeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Shutdown(shutdownCtx); err != nil {
		s.log().Error("shutdown after fatal error failed", "error", err)
	}

	// Exit process after shutdown attempt
	os.Exit(1)
}

func (s *Server) listenAddr() string {
	network := s.network()
	if network == NetworkUnix {
		return s.opts.Host
	}

	host := s.opts.Host
	if host == "" {
		host = defaultHost
	}

	// IPv6 addresses need brackets in host:port format.
	if network == NetworkTCP6 && host != "" && host[0] != '[' {
		return fmt.Sprintf("[%s]:%d", host, s.opts.Port)
	}
	return fmt.Sprintf("%s:%d", host, s.opts.Port)
}

func (s *Server) Shutdown(ctx context.Context) error {
	if !s.started.Load() {
		return ErrNotStarted
	}

	s.log().Info("stopping listener")

	// Stop accepting new connections.
	if s.ln != nil {
		_ = s.ln.Close()
	}

	// Cancel all subsystem contexts (replication loop, backlog trimmer, etc.)
	if s.cancel != nil {
		s.cancel()
	}
	if s.primary != nil {
		_ = s.primary.Close()
	}

	// Wait for in-flight requests to finish, or context to cancel.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.log().Info("all connections drained")
	case <-ctx.Done():
		s.log().Warn("context canceled, forcing close")
		s.closeConnections()
		<-done // wait for handlers to exit after force close
	}

	var err error
	if s.db != nil {
		s.log().Info("closing database")
		err = s.db.Close()
	}
	if s.lock != nil {
		if lockErr := s.lock.Close(); lockErr != nil {
			if err == nil {
				err = lockErr
			} else {
				err = errors.Join(err, lockErr)
			}
		}
	}

	if s.metaFile != nil {
		_ = s.metaFile.Close()
	}

	s.started.Store(false)
	s.log().Info("shutdown complete")
	return err
}

func (s *Server) closeConnections() {
	// Force close active connections to unblock handlers.
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	if !s.isReplica {
		for _, rc := range s.replicas {
			close(rc.sendCh) // signal writer goroutine to exit
			_ = rc.conn.Close()
		}
	}
	s.mu.Unlock()
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}

		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Go(func() {
			s.handleRequest(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
		})
	}
}

func (s *Server) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return noopLogger
}
