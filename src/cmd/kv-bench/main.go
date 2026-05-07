package main

import (
	"context"
	"flag"
	"fmt"
	"kvgo/kvpb"
	"kvgo/protocol"
	"kvgo/transport"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	addr        = flag.String("addr", "127.0.0.1:4000", "server address")
	network     = flag.String("network", "tcp", "network type: tcp, tcp4, tcp6, unix")
	concurrency = flag.Int("c", 50, "number of concurrent connections")
	totalReqs   = flag.Int("n", 100000, "total number of requests")
	valueSize   = flag.Int("size", 128, "value payload size in bytes")
	getRatio    = flag.Float64("get-ratio", 0.0, "fraction of requests that are GETs (0.0-1.0)")
	warmup      = flag.Int("warmup", 1000, "warmup requests to discard from stats")
	timeout     = flag.Duration("timeout", 5*time.Second, "per-request timeout")
	pipeline    = flag.Bool("pipeline", false, "pipeline requests: overlap send and receive")
	useGRPC     = flag.Bool("grpc", false, "use gRPC protocol instead of binary")
)

type stats struct {
	mu        sync.Mutex
	latencies []time.Duration
	errors    int
}

func main() {
	flag.Parse()

	if *getRatio < 0 || *getRatio > 1 {
		fmt.Println("error: -get-ratio must be between 0.0 and 1.0")
		return
	}

	fmt.Printf("Benchmark: %d requests, %d connections, %d byte values, %.0f%% GETs, pipeline=%v, grpc=%v\n",
		*totalReqs, *concurrency, *valueSize, *getRatio*100, *pipeline, *useGRPC)
	fmt.Printf("Target: %s (%s)\n", *addr, *network)

	// Pre-generate data
	keys, values := prepareData(*totalReqs, *valueSize)

	// Run benchmark
	start := time.Now()
	var wg sync.WaitGroup
	st := &stats{latencies: make([]time.Duration, 0, *totalReqs)}

	reqsPerWorker := *totalReqs / *concurrency

	// For gRPC, share a single connection across all workers (like etcd's bench).
	// Each worker fires RPCs concurrently over the same HTTP/2 connection.
	var sharedGRPCClient kvpb.KVClient
	var sharedGRPCConn *grpc.ClientConn
	if *useGRPC {
		var err error
		sharedGRPCConn, err = grpc.NewClient(*addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			fmt.Printf("grpc dial failed: %v\n", err)
			return
		}
		defer sharedGRPCConn.Close()
		sharedGRPCClient = kvpb.NewKVClient(sharedGRPCConn)
	}

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		startIdx := i * reqsPerWorker
		endIdx := startIdx + reqsPerWorker
		if i == *concurrency-1 {
			endIdx = *totalReqs
		}

		go func(workerID, sIdx, eIdx int) {
			defer wg.Done()
			if *useGRPC {
				runGRPCWorker(workerID, keys[sIdx:eIdx], values[sIdx:eIdx], st, sharedGRPCClient)
			} else if *pipeline {
				runPipelinedWorker(workerID, keys[sIdx:eIdx], values[sIdx:eIdx], st)
			} else {
				runWorker(workerID, keys[sIdx:eIdx], values[sIdx:eIdx], st)
			}
		}(i, startIdx, endIdx)
	}

	wg.Wait()
	duration := time.Since(start)

	printReport(duration, st)
}

func runWorker(id int, keys []string, values [][]byte, st *stats) {
	conn, err := net.DialTimeout(*network, *addr, *timeout)
	if err != nil {
		fmt.Printf("worker %d: connect failed: %v\n", id, err)
		st.mu.Lock()
		st.errors += len(keys)
		st.mu.Unlock()
		return
	}
	defer conn.Close()

	t := transport.NewMultiplexedTransport(conn)
	defer t.Close()
	lats := make([]time.Duration, 0, len(keys))
	errs := 0

	for i := range keys {
		key := keys[i]
		val := values[i]

		// Decide operation based on getRatio
		var req protocol.Request
		if rand.Float64() < *getRatio {
			req = protocol.Request{Cmd: protocol.CmdGet, Key: []byte(key)}
		} else {
			req = protocol.Request{Cmd: protocol.CmdPut, Key: []byte(key), Value: val}
		}

		t0 := time.Now()

		payload, err := protocol.EncodeRequest(req)
		if err != nil {
			errs++
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)

		if err := t.Send(ctx, payload); err != nil {
			cancel()
			errs++
			continue
		}

		respPayload, err := t.Receive(ctx)
		cancel()
		if err != nil {
			errs++
			continue
		}

		resp, err := protocol.DecodeResponse(respPayload)
		if err != nil {
			errs++
			continue
		}

		// Count protocol-level errors but don't break
		if resp.Status == protocol.StatusError {
			errs++
			continue
		}

		lats = append(lats, time.Since(t0))
	}

	st.mu.Lock()
	st.latencies = append(st.latencies, lats...)
	st.errors += errs
	st.mu.Unlock()
}

func runPipelinedWorker(id int, keys []string, values [][]byte, st *stats) {
	conn, err := net.DialTimeout(*network, *addr, *timeout)
	if err != nil {
		fmt.Printf("worker %d: connect failed: %v\n", id, err)
		st.mu.Lock()
		st.errors += len(keys)
		st.mu.Unlock()
		return
	}
	defer conn.Close()

	t := transport.NewMultiplexedTransport(conn)
	defer t.Close()

	// Pre-encode all payloads
	payloads := make([][]byte, len(keys))
	sendTimes := make([]time.Time, len(keys))
	for i := range keys {
		var req protocol.Request
		if rand.Float64() < *getRatio {
			req = protocol.Request{Cmd: protocol.CmdGet, Key: []byte(keys[i])}
		} else {
			req = protocol.Request{Cmd: protocol.CmdPut, Key: []byte(keys[i]), Value: values[i]}
		}
		p, err := protocol.EncodeRequest(req)
		if err != nil {
			st.mu.Lock()
			st.errors += len(keys)
			st.mu.Unlock()
			return
		}
		payloads[i] = p
	}

	// Sender goroutine: fire all requests without waiting for responses
	sendErrs := make(chan int, 1)
	go func() {
		errs := 0
		for i := range payloads {
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			sendTimes[i] = time.Now()
			if err := t.Send(ctx, payloads[i]); err != nil {
				cancel()
				errs++
				continue
			}
			cancel()
		}
		sendErrs <- errs
	}()

	// Receiver: collect responses in order
	lats := make([]time.Duration, 0, len(keys))
	errs := 0
	for i := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		respPayload, err := t.Receive(ctx)
		cancel()
		if err != nil {
			errs++
			continue
		}

		resp, err := protocol.DecodeResponse(respPayload)
		if err != nil {
			errs++
			continue
		}

		if resp.Status == protocol.StatusError {
			errs++
			continue
		}

		lats = append(lats, time.Since(sendTimes[i]))
	}

	errs += <-sendErrs

	st.mu.Lock()
	st.latencies = append(st.latencies, lats...)
	st.errors += errs
	st.mu.Unlock()
}

func prepareData(n, size int) ([]string, [][]byte) {
	keys := make([]string, n)
	vals := make([][]byte, n)

	// Shared payload to reduce memory
	staticVal := make([]byte, size)
	for i := range staticVal {
		staticVal[i] = byte('a' + (i % 26))
	}

	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("bench:k%d", i)
		vals[i] = staticVal
	}
	return keys, vals
}

func printReport(d time.Duration, st *stats) {
	// Sort latencies for percentile calculation
	sort.Slice(st.latencies, func(i, j int) bool {
		return st.latencies[i] < st.latencies[j]
	})

	total := len(st.latencies)
	if total == 0 {
		fmt.Println("\nNo successful requests.")
		fmt.Printf("Errors: %d\n", st.errors)
		return
	}

	// Discard warmup from stats (if we have enough data)
	warmupCount := *warmup
	if warmupCount >= total {
		warmupCount = 0
	}
	lats := st.latencies[warmupCount:]
	n := len(lats)

	if n == 0 {
		fmt.Println("\nAll requests were warmup.")
		return
	}

	// Calculate stats
	var sum time.Duration
	for _, l := range lats {
		sum += l
	}
	avg := sum / time.Duration(n)
	min := lats[0]
	max := lats[n-1]

	p50 := lats[n/2]
	p99 := lats[min64(int64(float64(n)*0.99), int64(n-1))]
	p999 := lats[min64(int64(float64(n)*0.999), int64(n-1))]

	fmt.Println("\n--- Benchmark Results ---")
	fmt.Printf("Total Requests: %d (warmup: %d discarded)\n", total, warmupCount)
	fmt.Printf("Successful:     %d\n", total)
	fmt.Printf("Errors:         %d\n", st.errors)
	fmt.Printf("Duration:       %v\n", d)
	fmt.Printf("Throughput:     %.2f req/s\n", float64(total)/d.Seconds())
	fmt.Println("\nLatency (excluding warmup):")
	fmt.Printf("  Min:   %v\n", min)
	fmt.Printf("  Avg:   %v\n", avg)
	fmt.Printf("  Max:   %v\n", max)
	fmt.Printf("  P50:   %v\n", p50)
	fmt.Printf("  P99:   %v\n", p99)
	fmt.Printf("  P99.9: %v\n", p999)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func runGRPCWorker(id int, keys []string, values [][]byte, st *stats, client kvpb.KVClient) {
	lats := make([]time.Duration, 0, len(keys))
	errs := 0
	var err error

	for i := range keys {
		key := keys[i]
		val := values[i]

		t0 := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)

		if rand.Float64() < *getRatio {
			_, err = client.Get(ctx, &kvpb.GetRequest{Key: []byte(key)})
		} else {
			_, err = client.Put(ctx, &kvpb.PutRequest{Key: []byte(key), Value: val})
		}
		cancel()

		if err != nil {
			errs++
			continue
		}

		lats = append(lats, time.Since(t0))
	}

	st.mu.Lock()
	st.latencies = append(st.latencies, lats...)
	st.errors += errs
	st.mu.Unlock()
}
