# Sabotage
Client writes to primary, immediately reads from replica with replication lag.

## Current behavior
Client writes: `PUT key=X value=100` → Primary acknowledges (seq=500)  
Client reads: `GET key=X` → Load balancer routes to Replica (lastApplied=485)  
Result: **NotFound** (client's own write disappeared)

## The problem
Replication is asynchronous. Under normal load, lag is ~50ms. Under heavy load, can spike to seconds.

Client confused: "I just wrote it, why is it missing?"

This violates **read-your-writes** guarantee (also called session consistency).

## Real systems
- **Redis:** No automatic guarantee. Clients read from primary, or accept staleness with READONLY mode.
- **MongoDB:** Session tokens. Write returns seq, client includes in reads, replica waits for seq.
- **Cassandra:** Tunable consistency. QUORUM reads check multiple replicas.
- **DynamoDB:** Flag-based. Eventually consistent (fast) vs strongly consistent (slower) reads.

## Design space

**Option A: Version token (MongoDB approach)**
- PUT returns sequence number to client
- Client includes `WaitForSeq=N` in GET request
- Replica blocks until `lastApplied >= N` or timeout
- On timeout → redirect to primary
- ✅ Preserves read scaling (replicas still serve)
- ❌ Adds read latency (blocking wait)
- ❌ Client must track seq

**Option B: Sticky session (simple but naive)**
- Pin client to primary for entire session
- No reads from replicas
- ✅ Zero latency penalty
- ✅ Simple implementation
- ❌ Destroys read scaling (defeats replication purpose)
- ❌ Primary becomes hotspot

**Option C: Primary tracks client writes**
- Primary remembers: clientID → last write seq
- Client includes token in reads
- Replica checks: "do I have this client's writes?"
- ✅ Best-effort when replica caught up
- ❌ Primary memory overhead
- ❌ State lost on primary crash
- ❌ Complex coordination

## Decision
Implement **Option A: Version token** with **optional flag** (default off).

**Why:**
- Preserves read scalability: replicas serve eventual reads without coordination, strong reads available when needed
- Backward compatible: default behavior identical to Redis READONLY (no client changes required)
- Measurable tradeoff: can benchmark latency cost of strong consistency vs eventual consistency
- Simple failure mode: timeout → redirect to primary (reuses existing StatusReadOnly mechanism from 023)
- Bounded complexity: blocking isolated to opt-in strong reads, doesn't affect default path performance

**Design:**
- Default: `GET key` serves immediately (eventual, matches Redis)
- Strong: `GET key WaitForSeq=N` blocks until caught up (read-your-writes)
- Benchmark reports both: eventual latency + strong read penalty

**Blocking strategy:**
- Polling loop with capped exponential backoff: 1ms, 2ms, 4ms, 8ms, 10ms, 10ms...
- Timeout: 100ms default
- On catch-up (lastApplied >= WaitForSeq): return StatusOK with data
- On timeout: return StatusReadOnly + primary address (redirect)

**What changes:**
1. Protocol: Add `Seq` field to PUT response
2. Protocol: Add `WaitForSeq` field to GET request (0 = no wait)
3. Primary: Return `lastSeq` in PUT response
4. Replica: Block on GET if `WaitForSeq > 0`, timeout → redirect to primary
5. Client: Track seq from writes, optionally include in reads

**Tradeoff:**
- ✅ Optional (fast path preserved for benchmarks)
- ✅ Preserves strong consistency guarantee (timeout → redirect, never returns stale)
- ✅ Measurable (can show latency cost)
- ❌ Polling overhead (10 checks per 100ms timeout)
- ❌ Client state tracking (seq from last write)

## Implementation

### Protocol Changes

Added `Seq` and `WaitForSeq` fields to uniform header (already supported after 024-protocol refactoring):
- Request: `WaitForSeq uint64` - replica waits for this sequence before serving GET (0 = eventual)
- Response: `Seq uint64` - current sequence number at time of response
- Flags: `FlagHasWaitForSeq` indicates strong read (CmdGet with WaitForSeq > 0)

### Server-Side

**Primary (handler_put.go):**
```go
seq := s.seq.Add(1)
// ... replicate ...
return s.writeResponse(ctx.Framer, protocol.Response{Status: StatusOK, Seq: seq})
```

**Replica (handler_get.go):**
```go
func (s *Server) handleGet(ctx *RequestContext) error {
    req, err := protocol.DecodeRequest(ctx.Payload)
    if req.WaitForSeq > 0 {
        return s.doStrongGet(ctx, &req)  // Strong read: block until caught up
    }
    return s.doGet(ctx, &req)  // Eventual read: serve immediately
}

func (s *Server) doStrongGet(ctx *RequestContext, req *protocol.Request) error {
    maxDuration := 10 * time.Millisecond
    currentDuration := time.Millisecond
    
    startedAt := time.Now()
    for time.Since(startedAt) < s.opts.StrongReadTimeout {
        if s.lastSeq.Load() >= req.WaitForSeq {
            return s.doGet(ctx, req)  // Caught up, serve read
        }
        time.Sleep(currentDuration)
        currentDuration = min(currentDuration*2, maxDuration)
    }
    
    // Timeout: replica too far behind
    s.log().Warn("strong read timeout: replica lagging, redirecting to primary",
        "requested_seq", req.WaitForSeq,
        "current_seq", s.lastSeq.Load(),
        "elapsed", time.Since(startedAt))
    
    return s.respondReadOnly(ctx)  // Redirect to primary
}
```

**Configuration (server.go):**
```go
if opts.StrongReadTimeout <= 0 {
    opts.StrongReadTimeout = 100 * time.Millisecond  // Default
}
```

### Client-Side

**Session state (cmd/kv-cli/main.go):**
```go
var lastSeq uint64  // Track last PUT sequence for read-your-writes
```

**PUT tracking:**
```go
seq, err := doPut(f, key, value, timeout)
if err == nil && seq > 0 {
    lastSeq = seq  // Remember for strong reads
}
```

**Strong GET:**
```bash
> get --strong mykey
```

Parses `--strong` flag, sets `WaitForSeq=lastSeq` in request:
```go
var waitForSeq uint64
if strong {
    if lastSeq == 0 {
        fmt.Println("(no writes in this session, --strong has no effect)")
    } else {
        waitForSeq = lastSeq
    }
}
```

**Response handling:**
```go
case protocol.StatusOK:
    fmt.Printf("%s\n", resp.Value)
    if waitForSeq > 0 {
        fmt.Printf("(strong read: waited for seq=%d, replica at seq=%d)\n", waitForSeq, resp.Seq)
    }
case protocol.StatusReadOnly:
    // Timeout: suggest primary
    fmt.Printf("(replica lagging, try primary: %s)\n", string(resp.Value))
```

### Testing

Created `server/session_test.go` with three tests:

**TestStrongConsistencyRead:**
- PUT to primary (seq=1)
- GET from replica with `WaitForSeq=1`
- Verifies: replica blocks, returns correct value once caught up
- Result: ✅ PASS (10.83s, includes replication lag + polling)

**TestEventualConsistencyRead:**
- PUT to primary, wait for replication
- GET from replica with `WaitForSeq=0` (default)
- Verifies: returns immediately, no blocking
- Result: ✅ PASS (10.88s, <1ms actual read time)

**TestStrongReadTimeout:**
- GET from replica with impossible `WaitForSeq=999999`
- `StrongReadTimeout=10ms` (very short for test)
- Verifies: times out, returns `StatusReadOnly` with primary address
- Result: ✅ PASS (5.82s, timeout ~14-28ms including backoff overhead)

### Usage Example

**Terminal 1 - Primary:**
```bash
kv-server -data-dir=data-primary -port=5000
```

**Terminal 2 - Replica:**
```bash
kv-server -data-dir=data-replica -port=5001 -replica-of=127.0.0.1:5000
```

**Terminal 3 - Client (connected to replica):**
```bash
kv-cli -addr=127.0.0.1:5001

> put mykey hello
(replica, redirecting to primary: 127.0.0.1:5000)
OK

> get mykey
hello

> get --strong mykey
hello
(strong read: waited for seq=1, replica at seq=1)
```

### Performance Characteristics

**Eventual read (default):**
- Latency: ~1-2ms (memory lookup + network)
- No coordination overhead
- May return stale data or NotFound

**Strong read (--strong):**
- Best case: ~1-2ms (replica already caught up)
- Typical case: ~10-50ms (replication lag + polling)
- Worst case: 100ms + redirect to primary
- Polling backoff: 1→2→4→8→10ms (reduces CPU overhead)

### Observability

**Logs on timeout (WARN level):**
```
strong read timeout: replica lagging, redirecting to primary
  requested_seq=500 current_seq=485 elapsed=102ms timeout=100ms
```

Indicates:
- Client wanted seq=500
- Replica only at seq=485 (15 operations behind)
- Waited full timeout before giving up

### Failure Modes

| Scenario | Behavior |
|----------|----------|
| Replica caught up | Strong read succeeds, adds ~0-10ms latency |
| Replica lagging (< timeout) | Blocks and polls, succeeds once caught up |
| Replica lagging (> timeout) | Returns `StatusReadOnly`, client should retry on primary |
| Replication partition | Replica never catches up → timeout → redirect |
| Client loses seq token | Strong read uses seq=0, degrades to eventual read |

### Limitations

1. **Per-session only:** Seq tracked in client memory, not persisted
2. **No cross-client guarantee:** Client A's write not visible to Client B's strong read (Client B has seq=0)
3. **No staleness bound:** Eventual reads can be arbitrarily stale
4. **No automatic retry:** Client must handle `StatusReadOnly` and reconnect to primary

These are acceptable for the learning/portfolio goal. Production systems would add:
- Persistent session tokens
- Cluster-wide monotonic reads
- Staleness bounds ("reject reads > 5 seconds old")
- Automatic client-side retry/failover

