package main

import (
	"context"
	"fmt"
	"kvgo/engine"
)

func main() {
	db, err := engine.NewDB("../.test/src_main", context.Background())
	if err != nil {
		fmt.Printf("error: failed to create database: %v\n", err)
		return
	}

	key := "user:100"
	var val []byte

	fmt.Println("Writing data...")
	db.Put(key, val)

	fmt.Println("Reading data...")
	result, ok := db.Get(key)

	if ok {
		fmt.Printf("Found: %s\n", result)
	} else {
		fmt.Println("Not found!")
	}
}
