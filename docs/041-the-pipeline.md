# 041 — The Pipeline

040 fixed concurrent proposal loss with the stable/unstable split. `kv-bench` works at any concurrency — zero errors. But throughput doesn't scale.

```
kv-go single-node:

1 connection:   141 req/s, P50 7.3ms
5 connections:  172 req/s, P50 29ms
50 connections: 171 req/s, P50 291ms
```

50x the concurrency, 1.2x the throughput. Latency grows linearly with connections — 50 workers queue behind the same pipeline. The system is correct but the pipeline doesn't parallelize.

## Reasoning

### Where does the time go?

The Raft event loop is single-threaded. Each Ready/Advance cycle processes a batch of entries — persist, send, apply, advance. Throughput is `batch_size / cycle_time`. If batch size is 1, throughput is fixed regardless of connections. If batch size grows with connections, throughput scales. Which is it?

### Measuring batch size

The Raft state machine splits state transitions from side effects. The natural place to measure batch size is where side effects execute — `handleBatch`, which receives a `Ready` from the node loop. Instrumented it to log `len(rd.Entries)` and `len(rd.CommittedEntries)` per cycle.

Ran `kv-bench -n 100 -c 5` three times. All three runs show the same pattern ([log 1](files/041-bench-log-1.txt), [log 2](files/041-bench-log-2.txt), [log 3](files/041-bench-log-3.txt)):

```
entries=1 committed=0    ← first worker's request arrives alone
entries=4 committed=1    ← 4 more arrive during the first cycle
entries=0 committed=4    ← the 4 commit
...
entries=1 committed=1    ← steady state: one entry per cycle
entries=1 committed=1
entries=1 committed=1
```

Two phases. A burst at startup where multiple entries batch together (1+4=5, matching the 5 connections). Then convergence to `entries=1, committed=1` — one entry per cycle, regardless of connections.

Batch size is 1 in steady state. That explains the throughput ceiling: `1 / (1/170) ≈ 170 req/s`. Adding connections doesn't help because each cycle still carries one entry.

### Why the burst works

At startup, all 5 workers send their first request simultaneously. Nobody is waiting for a previous response. Multiple proposals hit `propc` before the first Ready cycle fires. The first gets its own cycle (`entries=1`), and 4 more arrive during that cycle's persist/advance window (`entries=4`).

This proves the pipeline *can* batch. The convergence to 1 is not a pipeline limitation — it's something about the steady-state request pattern.

### Why steady state converges to 1

Each bench worker follows a strict loop: send → wait for response → send next. The response doesn't come until the entry completes the full round-trip (propose → commit → apply → respond). By the time a worker gets its response and sends the next request, the pipeline has already processed someone else's single request. The workers take turns instead of overlapping.

But wait — the server uses fire-and-forget + `wait.Wait`. `proposePut` registers a waiter, fires `Propose` (returns after `r.Step`, not after commit), then parks. Multiple goroutines *can* call `Propose` concurrently. So why don't proposals accumulate?

Because the node loop fires Ready after each one. It processes one `propc` per `select` iteration, `HasReady` returns true (the entry is in unstable), and Ready fires immediately with 1 entry. Unlike the burst phase — where all workers fire before the first Ready cycle — in steady state the goroutines are spread across the wake-up/IO/propose path and arrive at `propc` one at a time.

At this point I suspected the node loop needed a drain mechanism — collect multiple proposals before building Ready. But before building a fix, I needed to know: does etcd hit the same throughput ceiling?

### What etcd does

Ran etcd's benchmark tool (`tools/benchmark`, built from source via Docker — see `benchmarks/etcd-bench/`) against a single-node etcd on the same machine:

```
etcd single-node:

1 conn:   627 req/s, P50 1.5ms
5 conns:  2103 req/s, P50 2.3ms
50 conns: 7311 req/s, P50 6.9ms
```

etcd scales — 627 to 7311 req/s (11.7x) with 50x the connections. Same single-thread event loop, same `HasReady` check, same one-`propc`-per-select. No drain loop.

etcd's node loop comment (raft/node.go:355) explains:

> "We could instead force the previous Ready to be handled first, but it's generally good to emit larger Readys plus it simplifies testing."

After populating `rd` and arming `readyc`, the `select` has both `readyc <- rd` and `propc` ready. Go picks randomly — if it picks `propc`, another proposal is appended before Ready is sent. Batching is probabilistic, not forced.

Our own logs confirm this. In [log 2](files/041-bench-log-2.txt), line 11 shows `entries=2` in the transition zone between burst and steady state — a brief flash where two proposals were pending and the `select` picked `propc` before `readyc`. The mechanism works; the problem is that in steady state, there's only ever one proposal pending when the `select` fires.

But that's not the full story. The probabilistic batching only works if proposals are *available* at the `propc` channel when the `select` fires. etcd's benchmark tool pipelines requests — it sends multiple requests per gRPC connection without waiting for each response. So many proposals are in-flight simultaneously, and the `propc` channel always has someone waiting.

### The real bottleneck

The etcd observation pointed at the bench tool: etcd's bench pipelines, ours blocks. So the hypothesis was that pipelining `kv-bench` — separating send and receive into two goroutines — would let proposals accumulate on `propc`. Built a `-pipeline` flag that separates send and receive into two goroutines per worker. Ran it:

```
kv-go single-node (blocking):

1 conn:   152 req/s, P50 6.2ms
5 conns:  175 req/s, P50 28.6ms
50 conns: 178 req/s, P50 282ms

kv-go single-node (pipelined):

1 conn:   149 req/s, P50 8.8s    ← queuing delay, not processing time
5 conns:  178 req/s, P50 14.3s
50 conns: 174 req/s, P50 14.6s
```

Same throughput. Pipelining the bench made no difference. The P50 in seconds is expected — the sender fires all 1000 requests per worker instantly, but the server drains them one-by-one. The median request (#500) was sent at t=0 but waits for 499 ahead of it at ~35 req/s per connection: 500/35 ≈ 14.3s. That's pure queuing delay, and it's the strongest evidence that the server serializes per-connection.

The server's `handleRequest` loop is sequential per connection: `Receive → handler (blocks until committed+applied) → Receive`. Even with pipelined sends, the server reads one request, calls `proposePut` (which blocks until the entry is committed and applied via `wait.Wait`), writes the response, and only then reads the next request. With 50 connections, there are 50 goroutines — but each has exactly ONE proposal in-flight, identical to blocking mode.

Why does this kill batching? After the applier triggers waiters, all goroutines wake up and race to propose again. But the goroutines need to: unblock from `proposePut` → write the response to the client → loop back to `handleRequest` → `Receive` the next request → decode → `proposePut` → `Propose` → send on `propc`. That path involves I/O and function calls. The node loop is nanoseconds. By the time the first goroutine reaches `propc`, the node loop has already built a Ready and armed `readyc`. One entry per cycle. The rest queue behind the next cycle.

This is the difference from etcd. gRPC processes each call in its own goroutine. Many proposals are in-flight concurrently — goroutine N's proposal doesn't wait for goroutine N-1's response. Proposals accumulate on `propc` continuously. The Go `select` randomly picks `propc` vs `readyc`, building bigger batches probabilistically.

### The 3-node signal

To complete the picture, ran `kv-bench` against a 3-node localhost cluster:

```
kv-go 3-node (localhost):

1 conn:  10 req/s, P50 100ms
5 conns: 28 req/s, P50 18ms (679 errors)
50 conns: 7.8 req/s, P50 23ms (944 errors)
```

Two separate problems surfaced — neither related to batching:

1. **P50 100ms** matches the heartbeat interval. Commits may be gated by heartbeat ticks rather than immediate MsgAppResp round-trips.
2. **High error rates** under concurrency — the multi-node path has bugs.

These are future episodes, not this one.

## Shape

The diagnosis is complete. The naive fix (pipelining `kv-bench`) was built and tested — it didn't help because the bottleneck is server-side: `handleRequest` serializes per connection.

The real fix requires concurrent per-connection request handling. gRPC gives this for free — each RPC gets its own goroutine, HTTP/2 multiplexes responses, and the etcd comparison becomes apples-to-apples. That's episode 042.

What 041 ships:
- `kv-bench -pipeline` flag (naive fix, useful later when the server can handle concurrent requests)
- This doc as the diagnosis record

## Minimum tests

No new invariants to test — 041 is a diagnosis episode. The `-pipeline` flag was validated by benchmark (same throughput, zero errors).

## Open threads

1. **042: gRPC** — replace the binary protocol's client-facing API with gRPC. Each RPC gets its own goroutine, proposals overlap on `propc`, throughput scales. The etcd comparison becomes apples-to-apples.
2. **Heartbeat-gated commit** — 3-node P50 of 100ms suggests commits wait for the heartbeat cycle rather than immediate MsgAppResp. The leader should send MsgApp on propose, not on heartbeat tick.
3. **3-node error rate** — 679/1000 errors at 5 connections. The multi-node propose/forward path has bugs under load.
4. **Server-side batching** — multiple client requests encoded into one Raft entry. More aggressive than per-request pipelining but requires the apply path to map one committed entry to multiple client responses.
5. **Pipeline Ready/Advance** — etcd's async storage writes allow the next Ready to be built while the previous one is being persisted. Requires `acceptInProgress` / `offsetInProgress` from 040's open thread #2.

## Benchmark results

All runs: single-node kv-go on localhost, 5000 requests, 128-byte values, PUT-only.

### kv-go blocking (`kv-bench`)

| Conns | Throughput | P50      | Errors |
|-------|-----------|----------|--------|
| 1     | 152 req/s | 6.2ms    | 0      |
| 5     | 175 req/s | 28.6ms   | 0      |
| 50    | 178 req/s | 282.2ms  | 0      |

### kv-go pipelined (`kv-bench -pipeline`)

| Conns | Throughput | P50      | Errors |
|-------|-----------|----------|--------|
| 1     | 149 req/s | 8.8s     | 0      |
| 5     | 178 req/s | 14.3s    | 0      |
| 50    | 174 req/s | 14.6s    | 0      |

Pipelining the bench didn't change throughput. P50 is in seconds because the sender fires all requests instantly while the server drains them one-by-one — the median request sits in the TCP buffer waiting for ~500 requests ahead of it. That queuing delay is the proof: the server serializes per-connection.

### etcd (etcd benchmark tool, gRPC)

| Conns | Throughput  | P50    |
|-------|------------|--------|
| 1     | 627 req/s  | 1.5ms  |
| 5     | 2103 req/s | 2.3ms  |
| 50    | 7311 req/s | 6.9ms  |

etcd scales 11.7x because gRPC handles each call in its own goroutine — many proposals overlap on `propc`, enabling probabilistic batching via Go's `select`.
