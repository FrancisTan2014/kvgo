package utils

import (
	"crypto/rand"
	"fmt"
)

func GenerateUniqueID() string {
	b := make([]byte, 20) // 20 bytes = 40 hex chars
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate replid: %v", err))
	}
	return fmt.Sprintf("%x", b)
}

func ComputeQuorum(n int) int {
	return n/2 + 1
}
