package engine

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"sync"
	"time"
)

const numShards = 256 // lock striping: reduce contention vs 1 global mutex

var (
	ErrClosed  = errors.New("db is closed")
	noopLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// Options configures the database.
type Options struct {
	// SyncInterval controls how often the WAL is fsynced.
	// Lower = lower latency, higher = better throughput.
	// Zero means use DefaultSyncInterval (100ms).
	SyncInterval time.Duration
	Logger       *slog.Logger
}

type DB struct {
	shards []*shard

	ctx    context.Context
	cancel context.CancelFunc

	stateMu sync.RWMutex // shutdown gate: prevents Put/Close race (no enqueue after drain)
	closed  bool         // protected by stateMu

	closeOnce sync.Once
	closeErr  error

	// FatalErr is closed when a critical error occurs (e.g., WAL write failure).
	// Server should monitor this and initiate graceful shutdown.
	FatalErr chan struct{}

	opts Options
}

// NewDB opens or creates a database at path with default options.
func NewDB(path string, ctx context.Context) (*DB, error) {
	return NewDBWithOptions(path, Options{}, ctx)
}

// NewDBWithOptions opens or creates a database at path with the given options.
func NewDBWithOptions(path string, opts Options, ctx context.Context) (*DB, error) {
	var err error

	// Create root context for all shards - canceling this cancels all shard contexts
	rootCtx, rootCancel := context.WithCancel(ctx)

	// Create fatal error channel before shards (shared across all shards)
	fatalErrCh := make(chan struct{})

	shards := make([]*shard, numShards)
	for i := range shards {
		shards[i], err = newShard(path, i, opts.SyncInterval, rootCtx, fatalErrCh, opts.Logger)
		if err != nil {
			rootCancel()
			return nil, err
		}
	}

	db := &DB{
		shards:   shards,
		ctx:      rootCtx,
		cancel:   rootCancel,
		FatalErr: fatalErrCh,
		opts:     opts,
	}

	if err := db.replay(); err != nil {
		return nil, err
	} else {
		db.startWorkers()
	}

	return db, nil
}

func (db *DB) replay() error {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()

	var mu sync.Mutex
	var compoundErr error
	var wg sync.WaitGroup
	for _, s := range db.shards {
		wg.Go(func() {
			if err := s.replay(); err != nil {
				mu.Lock()
				compoundErr = errors.Join(compoundErr, err)
				mu.Unlock()
				db.log().Error("Error occurred on replaying %s: %v", s.wal.indexFilename, err)
			}
		})
	}

	wg.Wait()
	db.log().Info("Replayed successfully")

	return compoundErr
}

func (db *DB) startWorkers() {
	for _, s := range db.shards {
		s.startBackgroundWorker()
	}
}

func (db *DB) Put(key string, value []byte) error {
	s := db.getShard(key)
	return s.put(key, value)
}

func (db *DB) Get(key string) ([]byte, bool) {
	s := db.getShard(key)
	data, ok, err := s.get(key)
	if err != nil {
		// Read errors (corruption, I/O failures) are logged but not returned.
		// Callers see (nil, false) for any read failure—same as missing key.
		// Operators detect issues via logs.
		db.log().Error("read failed", "key", key, "error", err)
	}
	return data, ok
}

// Clear deletes all keys from memory and resets the WAL file.
func (db *DB) Clear() error {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()

	var mu sync.Mutex
	var compoundErr error
	var wg sync.WaitGroup
	for _, s := range db.shards {
		wg.Go(func() {
			if err := s.clear(); err != nil {
				mu.Lock()
				compoundErr = errors.Join(compoundErr, err)
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if compoundErr != nil {
		return compoundErr
	}

	return nil
}

func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		db.stateMu.Lock()
		db.closed = true
		db.stateMu.Unlock()

		// Cancel root context - automatically cancels all shard contexts
		if db.cancel != nil {
			db.cancel()
		}

		// Stop all shards' background workers and close WAL files
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, s := range db.shards {
			wg.Go(func() {
				if err := s.stopBackgroundWorker(); err != nil {
					mu.Lock()
					if db.closeErr == nil {
						db.closeErr = err
					} else {
						db.closeErr = errors.Join(db.closeErr, err)
					}
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	})
	return db.closeErr
}

// Range calls fn sequentially for each key-value pair in the database.
// If fn returns false, iteration stops.
func (db *DB) Range(fn func(key string, value []byte) bool) error {
	for _, s := range db.shards {
		if err := s.forEach(fn); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) Clean() error {
	for _, s := range db.shards {
		select {
		case <-db.ctx.Done():
			return ErrClosed
		default:
			if err := s.clean(); err != nil {
				return err
			}
		}
	}

	return nil
}

func (db *DB) getShard(key string) *shard {
	return db.shards[hash(key)%numShards]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func (db *DB) log() *slog.Logger {
	if db.opts.Logger != nil {
		return db.opts.Logger
	}
	return noopLogger
}
