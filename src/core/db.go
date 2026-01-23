package core

import (
	"hash/fnv"
	"io"
	"sync"
)

const NumShards = 256

type DB struct {
	shards []*shard
	wal    *WAL
}

func NewDB(path string) (*DB, error) {
	wal, err := NewWAL(path)
	if err != nil {
		return nil, err
	}

	shards := make([]*shard, NumShards)
	for i := range shards {
		shards[i] = newShard()
	}

	db := &DB{shards, wal}

	// Replay WAL to restore state (skip writing back to WAL)
	err = wal.Read(func(key, value []byte) error {
		db.putInternal(string(key), value)
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, err
	}

	return db, nil
}

func (db *DB) Put(key string, value []byte) error {
	if err := db.wal.Write([]byte(key), value); err != nil {
		return err
	}
	if err := db.wal.Sync(); err != nil {
		return err
	}

	db.putInternal(key, value)
	return nil
}

// putInternal writes to memory only (used by replay and Put)
func (db *DB) putInternal(key string, value []byte) {
	s := db.getShard(key)
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
}

func (db *DB) Get(key string) ([]byte, bool) {
	s := db.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	val, ok := s.data[key]
	return val, ok
}

func (db *DB) Close() error {
	return db.wal.Close()
}

type shard struct {
	data map[string][]byte
	mu   sync.RWMutex
}

func newShard() *shard {
	return &shard{
		data: make(map[string][]byte),
	}
}

func (db *DB) getShard(key string) *shard {
	return db.shards[hash(key)%NumShards]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
