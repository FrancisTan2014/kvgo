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
