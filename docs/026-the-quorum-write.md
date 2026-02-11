# Sabotage
Primary crashes 10ms after acknowledging write. Write never reached replicas.

## The failure
```
T=0:    Client → PUT key=X value=100 → Primary
T=5ms:  Primary → WAL persisted, responds StatusOK
T=7ms:  Primary → queues write to replica's sendCh (hasn't sent over network yet)
T=10ms: Primary crashes (disk catches fire)
T=15ms: Manual failover: kv-cli promote replica1
T=20ms: Client → GET key=X → New primary
        Result: NotFound
```

Client received `StatusOK`. The system promised "write succeeded." The write disappeared.

**This isn't a bug—it's the architecture.** Async replication gives fast ACKs but acknowledged writes can vanish.

## The constraint

How do I know a write is durable?

**Previous answer:** "It's on disk (primary's WAL)."
**Problem:** That disk can die. Machine can crash. Data center can flood.

With 3 replicas, after PUT returns, the write exists on:
- Primary's disk: ✅
- Replica1's memory (queued in sendCh): maybe
- Replica2's disk: ❌ (hasn't received it yet)
- Replica3's network buffer: ❌

If primary crashes now, write is **gone**.

**The question:** How many nodes must have the write before acknowledging the client?

Options explored:
- 0 replicas (current): Fast. Loses acknowledged writes.
- 1 replica: Slightly safer. Loses if both primary + that replica crash.
- 2 replicas: Even safer. But what if there are only 2 replicas total?
- All replicas: Safest. But 1 slow/dead replica blocks all writes forever.
- ??? replicas: Some number that balances safety + availability?

**Partition scenario:** 5 nodes split: [3 nodes | 2 nodes]

If "all replicas must ACK" is required, neither side can accept writes (split-brain deadlock).
If "1 replica must ACK" is required, both sides can accept writes (split-brain divergence).

What number prevents split-brain writes while keeping the system available?

## What real systems do

Researched how production systems handle this:

- **Redis:** Async by default. Optional `WAIT N` command blocks until N replicas ACK.
- **MongoDB:** `w=1` (async) or `w=majority` (wait for majority of nodes).
- **Cassandra:** Tunable per request. `ONE` (any node), `QUORUM` (majority), `ALL` (all nodes).
- **etcd/Zookeeper:** Always require majority (via Raft/Paxos consensus). No async mode.

**Pattern:** They converge on **quorum** (majority acknowledgment) for durable writes.

**Quorum = ⌊N/2⌋ + 1**
- 3 nodes → quorum = 2
- 5 nodes → quorum = 3  
- 7 nodes → quorum = 4

Why majority? It's the only number where two groups can't both reach quorum during partition. Mathematical guarantee against split-brain.

## Verifying the math

Testing with 5-node partition: [3 nodes | 2 nodes]

**If we required N=1 (any single replica):**
```
- Left side (3 nodes): Has primary + replicas → accepts writes ✅
- Right side (2 nodes): If one thinks it's primary → also accepts writes ⚠️
```
Split-brain: Both sides can reach N=1. Conflicting writes.

**If we required N=4 (all replicas):**
```
- Left side (3 nodes): Can't reach 4 nodes → rejects writes ❌
- Right side (2 nodes): Can't reach 4 nodes → rejects writes ❌
```
Deadlock: System unavailable during partition.

**If we required N=3 (majority of 5):**
```
- Left side (3 nodes): Reaches quorum (3) → accepts writes ✅
- Right side (2 nodes): Can't reach quorum (only has 2) → rejects writes ✅
```
Only one side can form majority. No split-brain. System remains available on majority side.

**Why can't both sides have majority?**
Proof by contradiction: If both groups had majority, then:
- Group A has > 50% of nodes
- Group B has > 50% of nodes
- Together: > 100% of nodes (impossible)

Therefore, at most ONE group can have majority during partition. This prevents split-brain writes.

Most systems let applications choose durability per write (cache vs payment).

## Design space

Implementation options for quorum writes:

**Option A: Hard-code quorum (etcd/Zookeeper approach)**
```go
replicas := len(s.replicas) + 1 // +1 for primary
quorum := replicas/2 + 1
s.waitForReplicas(quorum - 1) // -1 because primary already has it
```
- ✅ Mathematical guarantee: survives F failures in 2F+1 cluster
- ✅ Balances safety + availability (majority always reachable if >50% alive)
- ✅ Simpler: only one code path
- ❌ ALL writes are slow (even cache writes, session tokens)
- ❌ Can't compare async vs quorum performance

**Option B: Configurable per request (Redis/MongoDB/Cassandra approach)**
```go
type WriteMode int
const (
    Async WriteMode = iota  // default, current behavior
    Quorum                  // wait for majority
)
```
Client decides per write:
- `PUT session:abc` → async (fast, acceptable loss)
- `PUT payment:123 --quorum` → durable (slow, must survive)

- ✅ Flexible (application knows its data durability needs)
- ✅ Measures both: async baseline + coordination cost
- ✅ Matches production systems (Redis WAIT, MongoDB w=majority)
- ✅ Can benchmark and compare: "Async: 0.5ms, Quorum: 8ms"
- ❌ Two code paths (more implementation complexity)

**Option C: Arbitrary N (Redis WAIT N approach)**
```go
s.waitForReplicas(n) // client specifies any N
```
- ❌ N is arbitrary (why 2? why 3? no mathematical meaning)
- ❌ N=10 with 3 replicas → deadlock
- ❌ N=2 in 5-node cluster during partition → both sides could reach N=2 (split-brain)

## Decision

**Option B: Configurable per request** (async default, quorum opt-in).

**Why not Option A (always quorum)?**
Would miss measuring the tradeoff. "Quorum adds latency" vs "Quorum is 15x slower" are different levels of understanding. Need concrete numbers.

**Why not Option C (arbitrary N)?**
Misses the core insight: majority has a unique mathematical property. Any other number either allows split-brain (too small) or causes unnecessary unavailability (too large).

**What this explores:**
- Coordination cost: How much does waiting for majority actually cost? (measured)
- Split-brain prevention: Why can't two groups both form majority? (mathematical proof via partition testing)
- Design tradeoff: When to sacrifice latency for durability (application decides per write)
- Foundation for linearizability: Quorum writes + quorum reads = linearizability (Episode 027)

## Implementation

**Protocol change:**
- Add `RequireQuorum` flag to Request (reuses FLAGS mechanism from Episode 024)
- Client: `put key=X value=100 --quorum`

**Primary behavior:**
- If `RequireQuorum=false` (default): Current behavior (async ACK)
- If `RequireQuorum=true`: Block until ⌊N/2⌋ + 1 replicas acknowledge

**Replica acknowledgment:**
Currently replicas receive writes but never respond. Changes needed:
1. Replica sends ACK back to primary after applying write
2. Primary tracks: which replicas ACK'd this sequence number?
3. Primary counts: have we reached quorum yet?
4. Primary blocks client response until: `acksReceived >= quorum` OR timeout

**Timeout behavior:**
If quorum not reached in 500ms:
- Return StatusError to client
- Write is in limbo (some replicas have it, some don't)
- Expected behavior: system can't form quorum (partition or dead replicas)

**Threading challenge:**
`handlePut` blocks waiting for ACKs. ACKs arrive in different goroutine (serveReplica or receiveFromPrimary). 
Communication mechanism needed: channel, condition variable, or polling.

**Tests:**
1. 3 nodes, quorum write succeeds (2 ACKs received)
2. 3 nodes, 1 replica dead, quorum write succeeds (still have 2/3)
3. 3 nodes, 2 replicas dead, quorum write times out (can't reach 2/3)
4. Measure latency: async vs quorum (expect 10-20x difference)

## Known Limitations

**Discovered during implementation (Episode 026):**

1. **Dynamic quorum (not static cluster size):** Quorum calculated from currently connected replicas, not original cluster size.
   - Behavior: Kill 3 of 4 replicas → quorum drops from 3 to 2 → writes still succeed with 1 remaining replica
   - Trade-off: Better availability (cluster survives node failures) vs weaker safety (smaller partition can form quorum)
   - Future work: Explicit cluster configuration separate from connection state (Raft joint consensus)
