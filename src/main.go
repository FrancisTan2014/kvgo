package main

import (
	"fmt"
	"kvgo/core"
)

func main() {
	db := core.NewSimpleDB()

	key := "user:100"
	val := []byte("Hello World!")

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
