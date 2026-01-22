package core

import (
	"sync"
)

type SimpleDB struct {
	data map[string][]byte
	mu   sync.RWMutex
}

func NewSimpleDB() *SimpleDB {
	return &SimpleDB{
		data: make(map[string][]byte),
	}
}

func (db *SimpleDB) Put(key string, value []byte) {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.data[key] = value
}

func (db *SimpleDB) Get(key string) ([]byte, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	val, ok := db.data[key]
	return val, ok
}
