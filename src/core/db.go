package core

import (
	"sync"
)

type DB struct {
	data map[string][]byte
	mu   sync.RWMutex
}

func NewDB() *DB {
	return &DB{
		data: make(map[string][]byte),
	}
}

func (db *DB) Put(key string, value []byte) {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.data[key] = value
}

func (db *DB) Get(key string) ([]byte, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	val, ok := db.data[key]
	return val, ok
}
