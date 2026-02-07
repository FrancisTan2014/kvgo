package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	testDir := filepath.Join(os.TempDir(), "fsync-bench")
	os.MkdirAll(testDir, 0755)
	defer os.RemoveAll(testDir)

	testFile := filepath.Join(testDir, "test.dat")

	// Test with different write sizes
	sizes := []int{100, 1024, 4096, 16384}
	iterations := 100

	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i % 256)
		}

		f, err := os.OpenFile(testFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("Failed to open file: %v\n", err)
			return
		}

		// Warmup
		for i := 0; i < 10; i++ {
			f.Write(data)
			f.Sync()
		}

		// Benchmark
		latencies := make([]time.Duration, 0, iterations)
		start := time.Now()

		for i := 0; i < iterations; i++ {
			t0 := time.Now()
			f.Write(data)
			f.Sync()
			latencies = append(latencies, time.Since(t0))
		}

		duration := time.Since(start)
		f.Close()
		os.Remove(testFile)

		// Calculate stats
		var min, max, sum time.Duration
		min = latencies[0]
		max = latencies[0]
		for _, lat := range latencies {
			if lat < min {
				min = lat
			}
			if lat > max {
				max = lat
			}
			sum += lat
		}
		avg := sum / time.Duration(len(latencies))

		// Calculate P50, P99
		sortedLats := make([]time.Duration, len(latencies))
		copy(sortedLats, latencies)
		for i := 0; i < len(sortedLats); i++ {
			for j := i + 1; j < len(sortedLats); j++ {
				if sortedLats[i] > sortedLats[j] {
					sortedLats[i], sortedLats[j] = sortedLats[j], sortedLats[i]
				}
			}
		}
		p50 := sortedLats[len(sortedLats)/2]
		p99 := sortedLats[int(float64(len(sortedLats))*0.99)]

		throughput := float64(iterations) / duration.Seconds()

		fmt.Printf("\n=== Write %d bytes + fsync ===\n", size)
		fmt.Printf("Iterations: %d\n", iterations)
		fmt.Printf("Throughput: %.1f ops/sec\n", throughput)
		fmt.Printf("Latency:\n")
		fmt.Printf("  Min: %v\n", min)
		fmt.Printf("  Avg: %v\n", avg)
		fmt.Printf("  P50: %v\n", p50)
		fmt.Printf("  P99: %v\n", p99)
		fmt.Printf("  Max: %v\n", max)
	}

	fmt.Println("\n=== Summary ===")
	fmt.Println("If P50 < 1ms:  1ms sync interval is reasonable")
	fmt.Println("If P50 < 5ms:  5ms sync interval is reasonable")
	fmt.Println("If P50 > 10ms: 10ms sync interval is conservative (your current setting)")
}
