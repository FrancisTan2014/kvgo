package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"kvgo/engine"
	"kvgo/protocol"
	"kvgo/raft"
	"kvgo/raftpb"
	"kvgo/transport"
	rafttransport "kvgo/transport/raft"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyStarted = errors.New("server: already started")
	ErrNotStarted     = errors.New("server: not started")
	ErrDBInUse        = errors.New("server: database is already in use")

	noopLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type Options struct {
	ID    uint64 // unique node ID
	Peers []*rafttransport.PeerInfo

	Protocol     string       // ProtocolTCP (default); future: QUIC, gRPC, etc.
	Network      string       // NetworkTCP, NetworkTCP4, NetworkTCP6, or NetworkUnix
	Host         string       // address, or socket path when Network is NetworkUnix
	Port         uint16       // ignored when Network is NetworkUnix
	RaftPort     uint16       // Raft channel
	RaftListener net.Listener // optional: pre-bound raft listener (for tests with :0 ports)
	DataDir      string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxFrameSize int

	// SyncInterval controls how often the WAL is fsynced (latency vs throughput tradeoff).
	// Lower values reduce latency but increase fsync overhead.
	// Zero means use engine.DefaultSyncInterval (100ms).
	SyncInterval time.Duration

	Logger *slog.Logger // optional debug logger; nil disables logging
}

type StateMachine interface {
	Get(key string) ([]byte, bool)
	Put(key string, value []byte) error
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

	wg sync.WaitGroup

	// Raft subsystem
	raftHost      RaftHost
	raftTransport RaftTransporter
	raftStorage   *raft.DurableStorage
	sm            StateMachine
	w             Wait
	reqIDGen      atomic.Uint64
}

func validate(opts *Options) error {
	if opts.DataDir == "" {
		return fmt.Errorf("server: DataDir is required")
	}
	if opts.Network != "" && !supportedNetworks[opts.Network] {
		return fmt.Errorf("server: unsupported Network %q", opts.Network)
	}
	if opts.Network == NetworkUnix && opts.Host == "" {
		return fmt.Errorf("server: Host (socket path) is required for %s network", NetworkUnix)
	}
	if opts.ID == 0 {
		return errors.New("server: ID cannot be zero")
	}
	return nil
}

func NewServer(opts Options) (*Server, error) {
	if err := validate(&opts); err != nil {
		return nil, err
	}
	opts.applyDefaults()

	s := &Server{
		opts:            opts,
		conns:           make(map[transport.StreamTransport]struct{}),
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
		w:               newWait(),
	}

	s.registerRequestHandlers()

	if err := s.initializeRaftHost(); err != nil {
		return nil, err
	}

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
			if s.cancel != nil {
				s.cancel()
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
	s.db = db
	s.sm = s.db

	ln, err = net.Listen(s.network(), s.listenAddr())
	if err != nil {
		return err
	}

	s.lock, s.ln = lock, ln

	go s.acceptLoop()
	go s.monitorDBHealth()

	if s.raftTransport == nil || s.raftHost == nil {
		return errors.New("server: raft subsystem not initialized")
	}
	if err = s.raftTransport.Start(); err != nil {
		return err
	}
	s.raftHost.Start()

	s.wg.Go(func() { s.run() })

	return nil
}

func (s *Server) initializeRaftHost() error {
	rtc := rafttransport.RaftTransportConfig{
		ListenAddr: fmt.Sprintf("%s:%d", s.opts.Host, s.opts.RaftPort),
		Listener:   s.opts.RaftListener,
	}
	rt, err := rafttransport.NewRaftTransport(rtc, s, s.log().WithGroup("rafttransport"))
	if err != nil {
		return err
	}

	pids := make([]uint64, len(s.opts.Peers))
	for i, p := range s.opts.Peers {
		pids[i] = p.ID
		rt.AddPeer(p.ID, p.Addr)
	}

	rs, err := raft.NewDurableStorage(s.opts.DataDir)
	if err != nil {
		return err
	}
	s.raftStorage = rs

	s.raftHost, err = NewRaftHost(RaftHostConfig{
		ID:        s.opts.ID,
		Peers:     pids,
		Storage:   rs,
		Transport: rt,
	})
	if err != nil {
		return err
	}

	s.raftTransport = rt
	return nil
}

func (s *Server) Process(ctx context.Context, m *raftpb.Message) error {
	return s.raftHost.Step(ctx, m)
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

	// Cancel all subsystem contexts.
	if s.cancel != nil {
		s.cancel()
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
		s.wg.Wait()
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

	if s.raftHost != nil {
		s.raftHost.Stop()
	}
	if s.raftTransport != nil {
		s.raftTransport.Stop()
	}
	if s.raftStorage != nil {
		_ = s.raftStorage.Close()
	}

	s.started.Store(false)
	s.log().Info("shutdown complete")
	return err
}

func (s *Server) closeConnections() {
	s.connMu.Lock()
	for c := range s.conns {
		_ = c.Close()
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
			s.handleRequest(t, s.opts.ReadTimeout)
			s.connMu.Lock()
			delete(s.conns, t)
			s.connMu.Unlock()
			_ = t.Close()
		})
	}
}

func (s *Server) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return noopLogger
}

func (s *Server) run() {
	applyc := s.raftHost.Apply()
	for {
		select {
		case <-s.ctx.Done():
			return
		case ap := <-applyc:
			s.applyBatch(ap)
		}
	}
}

func (s *Server) applyBatch(ap toApply) {
	for _, raw := range ap.data {
		s.applyEntry(raw)
	}
}

func (s *Server) applyEntry(raw []byte) {
	id, payload, err := unmarshalEnvelope(raw)
	if err != nil {
		s.log().Warn("apply: malformed envelope", "error", err)
		return
	}

	req, err := protocol.DecodeRequest(payload)
	if err != nil {
		s.log().Warn("apply: decode failed", "error", err)
		s.w.Trigger(id, err)
		return
	}

	switch req.Cmd {
	case protocol.CmdPut:
		err := s.sm.Put(string(req.Key), req.Value)
		if err != nil {
			s.w.Trigger(id, err)
		} else {
			s.w.Trigger(id, nil)
		}
	default:
		s.log().Warn("apply: unknown command", "cmd", req.Cmd)
		s.w.Trigger(id, fmt.Errorf("unknown command in apply: %d", req.Cmd))
	}
}

func (s *Server) nextRequestID() uint64 {
	return s.reqIDGen.Add(1)
}
