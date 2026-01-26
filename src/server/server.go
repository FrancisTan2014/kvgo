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

var (
	ErrAlreadyStarted = errors.New("server: already started")
	ErrNotStarted     = errors.New("server: not started")
	ErrDBInUse        = errors.New("server: database is already in use")
)

type Options struct {
	Port    uint16
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

func (s *Server) Start() error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}

	lock, err := acquireDataDirLock(s.opts.DataDir)
	if err != nil {
		s.started.Store(false)
		if errors.Is(err, errLockBusy) {
			return ErrDBInUse
		}
		return err
	}

	walPath := filepath.Join(s.opts.DataDir, "wal.db")
	db, err := engine.NewDB(walPath)
	if err != nil {
		_ = lock.Close()
		s.started.Store(false)
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", s.opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = db.Close()
		_ = lock.Close()
		s.started.Store(false)
		return err
	}

	s.db = db
	s.lock = lock
	s.ln = ln

	go s.acceptLoop()
	return nil
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
