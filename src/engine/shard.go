package engine

import (
	"hash/fnv"
	"sync"
)

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
	return db.shards[hash(key)%numShards]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
