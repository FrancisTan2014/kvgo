package server

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"kvgo/engine"
	"kvgo/protocol"
	"kvgo/transport"
	"kvgo/utils"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
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

var (
	ErrAlreadyStarted = errors.New("server: already started")
	ErrNotStarted     = errors.New("server: not started")
	ErrDBInUse        = errors.New("server: database is already in use")

	noopLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type Options struct {
	Protocol  string // ProtocolTCP (default); future: QUIC, gRPC, etc.
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

	QuorumWriteTimeout time.Duration // default 500ms, how long to wait for quorum ACKs before timeout (Episode 026)
	QuorumReadTimeout  time.Duration // default 500ms

	// SyncInterval controls how often the WAL is fsynced (latency vs throughput tradeoff).
	// Lower values reduce latency but increase fsync overhead.
	// Zero means use engine.DefaultSyncInterval (100ms).
	SyncInterval time.Duration

	// Staleness bounds (Episode 025): replica rejects reads when exceeding thresholds
	ReplicaStaleHeartbeat time.Duration // time-based: reject if no heartbeat for this duration (partition detection), default 1s
	ReplicaStaleLag       int           // sequence-based: reject if > N operations behind (backlog limit), default 1000

	Logger *slog.Logger // optional debug logger; nil disables logging
}

type quorumWriteState struct {
	mu            sync.Mutex // Per-request lock
	needed        int
	ackCount      int32 // Protected by mu
	ackCh         chan struct{}
	closeOnce     sync.Once
	failed        atomic.Bool         // true if NACK received
	ackedReplicas map[string]struct{} // Track which replicas ACK'd by address (protected by mu)
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
	connMu sync.RWMutex
	connWg sync.WaitGroup
	conns  map[transport.StreamTransport]struct{}

	// Request dispatch
	requestHandlers map[protocol.Cmd]HandlerFunc

	// Replication state (primary role)
	seq      atomic.Uint64 // monotonic sequence number for writes
	replicas map[string]*replicaConn
	replid   string

	// Replication state (replica role)
	primary       transport.StreamTransport
	primaryNodeID string        // primary's nodeID (set from TOPOLOGY, used for ACK via peer channel)
	lastSeq       atomic.Uint64 // last applied sequence number
	lastHeartbeat time.Time     // updated from heartbeat messages
	primarySeq    uint64        // primary's position from heartbeat; compared with lastSeq for staleness detection

	// Replication loop control
	replCtx    context.Context
	replCancel context.CancelFunc

	// Quorum control
	quorumMu     sync.RWMutex
	quorumWrites map[string]*quorumWriteState
	quorumAckCh  chan string // Quorum ack channel

	// Cluster management
	peerManager *PeerManager
	nodeID      string
	term        atomic.Uint64
	votedFor    string
	role        atomic.Uint32
	roleChanged chan struct{}
	roleMu      sync.Mutex
	fenced      atomic.Bool // set on quorum-loss step-down; cleared when connected to a real leader

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
	opts.applyDefaults()

	s := &Server{
		opts:            opts,
		conns:           make(map[transport.StreamTransport]struct{}),
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
		replicas:        make(map[string]*replicaConn),
		lastHeartbeat:   time.Now(), // Initialize heartbeat timer (updated by PING messages)
		quorumAckCh:     make(chan string),
		quorumWrites:    make(map[string]*quorumWriteState),
		roleChanged:     make(chan struct{}),
	}

	if opts.ReplicaOf == "" {
		s.role.Store(uint32(RoleLeader))
	}

	s.peerManager = NewPeerManager(
		DialPeer(opts.Protocol, s.network(), opts.ReadTimeout),
		s.log(),
	)

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

	if s.nodeID == "" {
		s.nodeID = utils.GenerateUniqueID()
	}

	if s.replid == "" {
		// First boot as primary — reuse nodeID as the initial replication lineage ID.
		// A separate replid is only needed after failover (new primary, new lineage).
		s.replid = s.nodeID
	}

	if s.term.Load() == 0 && s.isLeader() {
		s.term.Store(1)
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
	if !s.isLeader() {
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
	go s.heartbeatLoop()
	go s.fenceLoop()
	go s.reconcileLoop()

	return nil
}

// pingTimeout returns the timeout for heartbeat pings.
func (s *Server) pingTimeout() time.Duration {
	return s.opts.ReadTimeout
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
		s.connWg.Wait()
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

	s.peerManager.Close()

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
	s.connMu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	for _, rc := range s.replicas {
		rc.connected.Store(false) // gate off senders before closing channel
		close(rc.sendCh)          // signal writer goroutine to exit
		_ = rc.transport.Close()
	}
	s.connMu.Unlock()
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}

		// Wrap immediately in transport
		t := transport.NewStreamTransport(s.opts.Protocol, conn)

		s.connMu.Lock()
		s.conns[t] = struct{}{}
		s.connMu.Unlock()

		s.connWg.Go(func() {
			takenOver := s.handleRequest(t, s.opts.ReadTimeout)
			s.connMu.Lock()
			delete(s.conns, t)
			s.connMu.Unlock()
			if !takenOver {
				_ = t.Close()
			}
			// If takenOver=true, connection ownership transferred to handler (e.g., replication)
		})
	}
}

func (s *Server) getReplicaSnapshot() map[string]*replicaConn {
	s.connMu.RLock()
	defer s.connMu.RUnlock()

	snapshot := make(map[string]*replicaConn, len(s.replicas))
	for id, rc := range s.replicas {
		snapshot[id] = rc
	}
	return snapshot
}

func (s *Server) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return noopLogger
}
