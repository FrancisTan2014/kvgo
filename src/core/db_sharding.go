package core

import (
	"hash/fnv"
	"sync"
)

const NumShards = 256

type ShardingDB struct {
	shards []*shard
}

func NewShardedDB() *ShardingDB {
	return &ShardingDB{
		shards: make([]*shard, NumShards),
	}
}

func (db *ShardingDB) Put(key string, value []byte) {
	bucket := db.getShard(key)
	if bucket == nil {
		return
	}

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	bucket.data[key] = value
}

func (db *ShardingDB) Get(key string) ([]byte, bool) {
	bucket := db.getShard(key)
	if bucket == nil {
		return nil, false
	}

	bucket.mu.RLock()
	defer bucket.mu.RUnlock()

	val, ok := bucket.data[key]
	return val, ok
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

func (db *ShardingDB) getShard(key string) *shard {
	hashCode := hash(key)
	slot := hashCode % NumShards
	return db.shards[slot]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
