package server

import (
	"errors"
	"fmt"
	"kvgo/engine"
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
	Network string // NetworkTCP, NetworkTCP4, NetworkTCP6, or NetworkUnix
	Host    string // address, or socket path when Network is NetworkUnix
	Port    uint16 // ignored when Network is NetworkUnix
	DataDir string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxFrameSize int
}

type Server struct {
	opts Options

	ln   net.Listener
	db   *engine.DB
	lock *dataDirLock

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup

	started atomic.Bool
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
	return &Server{opts: opts, conns: make(map[net.Conn]struct{})}, nil
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

	lock, err = acquireDataDirLock(s.opts.DataDir)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return ErrDBInUse
		}
		return err
	}

	walPath := filepath.Join(s.opts.DataDir, "wal.db")
	db, err = engine.NewDB(walPath)
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

func (s *Server) Shutdown() error {
	if !s.started.Load() {
		return ErrNotStarted
	}

	// Stop accepting new connections.
	if s.ln != nil {
		_ = s.ln.Close()
	}

	// Close active connections to unblock handlers.
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()

	var err error
	if s.db != nil {
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
	return err
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
