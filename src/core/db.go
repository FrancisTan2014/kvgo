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
	if err != nil && err != io.EOF {
		return nil, err
	}

	shards := make([]*shard, NumShards)
	for i := range shards {
		shards[i] = newShard()
	}

	db := &DB{shards, wal}
	// replay
	err = wal.Read(func(key []byte, value []byte) error {
		return db.Put(string(key), value)
	})

	if err != nil && err != io.EOF {
		return nil, err
	}

	return db, nil
}

func (db *DB) Put(key string, value []byte) error {
	bucket := db.getShard(key)
	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	err := db.wal.Write([]byte(key), value)
	if err != nil {
		return err
	}

	err = db.wal.Sync()
	if err != nil {
		return err
	}

	bucket.data[key] = value
	return nil
}

func (db *DB) Get(key string) ([]byte, bool) {
	bucket := db.getShard(key)
	bucket.mu.RLock()
	defer bucket.mu.RUnlock()

	val, ok := bucket.data[key]
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
	hashCode := hash(key)
	slot := hashCode % NumShards
	return db.shards[slot]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
