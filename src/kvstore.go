package main

type KVStore interface {
	Put(key string, value []byte)
	Get(key string) ([]byte, bool)
}
