package server

import (
	"context"
	"errors"
	"fmt"
	"kvgo/engine"
	"kvgo/protocol"
	"log"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultHost    = "127.0.0.1" // localhost only; override with Options.Host
	defaultNetwork = NetworkTCP
)

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
)

type Options struct {
	Network   string // NetworkTCP, NetworkTCP4, NetworkTCP6, or NetworkUnix
	Host      string // address, or socket path when Network is NetworkUnix
	Port      uint16 // ignored when Network is NetworkUnix
	ReplicaOf string // non-empty value indicates that it is a follower
	DataDir   string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxFrameSize int

	// SyncInterval controls how often the WAL is fsynced (latency vs throughput tradeoff).
	// Lower values reduce latency but increase fsync overhead.
	// Zero means use engine.DefaultSyncInterval (100ms).
	SyncInterval time.Duration

	Logger *log.Logger // optional debug logger; nil disables logging
}

type Server struct {
	opts      Options
	isReplica bool

	ln   net.Listener
	db   *engine.DB
	lock *dataDirLock

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	replicas map[net.Conn]*replicaConn
	wg       sync.WaitGroup

	started atomic.Bool
}

// replicaConn manages a single replica connection with a dedicated write goroutine.
type replicaConn struct {
	conn   net.Conn
	framer *protocol.Framer
	sendCh chan []byte // buffered channel for outgoing writes
}

const replicaSendBuffer = 1024 // max queued writes per replica

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

	isReplica := opts.ReplicaOf != ""
	var replicas map[net.Conn]*replicaConn
	if !isReplica {
		replicas = make(map[net.Conn]*replicaConn)
	}

	return &Server{
		opts:      opts,
		conns:     make(map[net.Conn]struct{}),
		isReplica: isReplica,
		replicas:  replicas,
	}, nil
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

	// Primary node just skips this roughly
	if err = s.connectToPrimary(); err != nil {
		return err
	}

	lock, err = acquireDataDirLock(s.opts.DataDir)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return ErrDBInUse
		}
		return err
	}

	walPath := filepath.Join(s.opts.DataDir, "wal.db")
	db, err = engine.NewDBWithOptions(walPath, engine.Options{
		SyncInterval: s.opts.SyncInterval,
	})
	if err != nil {
		return err
	}

	ln, err = net.Listen(s.network(), s.listenAddr())
	if err != nil {
		return err
	}

	s.lock, s.db, s.ln = lock, db, ln
	go s.acceptLoop()
	return nil
}

func (s *Server) network() string {
	if s.opts.Network == "" {
		return defaultNetwork
	}
	return s.opts.Network
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

	s.logf("shutdown: stopping listener")

	// Stop accepting new connections.
	if s.ln != nil {
		_ = s.ln.Close()
	}

	// Wait for in-flight requests to finish, or context to cancel.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logf("shutdown: all connections drained")
	case <-ctx.Done():
		s.logf("shutdown: context canceled, forcing close")
		s.closeConnections()
		<-done // wait for handlers to exit after force close
	}

	var err error
	if s.db != nil {
		s.logf("shutdown: closing database")
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

	s.started.Store(false)
	s.logf("shutdown: complete")
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

		s.logf("accepted connection from %s", conn.RemoteAddr())

		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Go(func() {
			s.handleRequest(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
			s.logf("closed connection from %s", conn.RemoteAddr())
		})
	}
}

func (s *Server) connectToPrimary() error {
	if s.opts.ReplicaOf == "" {
		return nil
	}

	s.logf("connecting to primary at %s", s.opts.ReplicaOf)

	conn, err := net.DialTimeout(s.network(), s.opts.ReplicaOf, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect to primary %s: %w", s.opts.ReplicaOf, err)
	}

	s.logf("connected to primary, sending replicate handshake")

	// Send replicate handshake to register as a replica.
	f := protocol.NewConnFramer(conn)
	req := protocol.Request{Op: protocol.OpReplicate}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("encode replicate request: %w", err)
	}

	if err := f.Write(payload); err != nil {
		_ = conn.Close()
		return fmt.Errorf("send replicate request: %w", err)
	}

	// Wait for ack from primary.
	respPayload, err := f.Read()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("read replicate response: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("decode replicate response: %w", err)
	}

	if resp.Status != protocol.StatusOK {
		_ = conn.Close()
		return fmt.Errorf("primary rejected replication: status %d", resp.Status)
	}

	s.logf("replication handshake complete, receiving writes")

	// Receive forwarded writes from primary in background.
	s.wg.Go(func() {
		defer conn.Close()
		s.receiveFromPrimary(f)
	})

	return nil
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf(format, args...)
	}
}
