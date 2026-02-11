# Sabotage
Strong reads (--strong) block on ONE replica catching up. 
If that replica is slow/partitioned, you wait. 
If it's behind but lying about its seq, you get stale data.

## Current gaps
- Single replica = single point of truth (what if it's wrong?)
- No verification (trust whatever one replica says)
- Timeout → redirect to primary (gives up on replicas entirely)

**What breaks:**
```
Scenario: 3 replicas, one lagging badly
- Client: GET --strong from replica #3 (seq=485)
- Replica #3: waits... waits... times out → redirect to primary
- Meanwhile replica #1 (seq=500) and #2 (seq=500) are ready
- Result: Wasted 100ms waiting on slow replica instead of reading from healthy ones
```

**Question:** How do you get stronger guarantees than "trust one replica"?

---

## Real Systems

**Redis:** No quorum reads. `READONLY` on replicas = eventual. Can't verify across replicas.  
**etcd:** Linearizable reads go through leader (Raft guarantee). Serializable reads can be stale.  
**Cassandra:** `QUORUM` reads - read from majority, return latest by timestamp  
**MongoDB:** `readConcern: "majority"` - waits until majority has data  
**DynamoDB:** Strongly consistent reads - route to leader  

**Pattern:** Systems with consensus (etcd, DynamoDB) route to leader. Systems without (Cassandra) do quorum fan-out.

---

## Design Space

### Option A: Quorum Read (Simple Majority)
```
1. Fan out GET to all replicas (3 total)
2. Wait for majority responses (2 of 3)
3. Compare seq numbers, return highest
4. Timeout: 50ms (faster than strong read's 100ms)
```

**Tradeoff:**
- ✅ Survives one slow replica (majority still responds)
- ✅ Stronger than single-replica strong read (verified across replicas)
- ✅ Read repair opportunity (see newest value)
- ❌ ~3-5x latency vs eventual (network fan-out cost)
- ❌ More complex (parallel coordination)

**Quorum intersection property:**
```
Write: Wait for 2/3 ACKs before responding
Read:  Wait for 2/3 responses before trusting

Any write quorum (size 2) overlaps with any read quorum (size 2)
→ At least one replica in read quorum saw the write
→ Pick highest seq = guaranteed to get acknowledged writes
```

### Option B: Read Repair + Eventual
```
1. Serve from local replica (fast path)
2. Background: fan out to others, detect staleness
3. If mismatch found, async repair
```

**Tradeoff:**
- ✅ Fast path unchanged (eventual read speed)
- ✅ Self-healing over time
- ❌ Still serves stale data on first read
- ❌ Doesn't solve "I need strong guarantee NOW"

### Option C: Linear Quorum Read (Raft-style)
```
1. Contact leader for current commit index
2. Fan out to majority
3. Wait until majority confirms ≥ commit index
4. Return value
```

**Tradeoff:
- ✅ Linearizable (strongest guarantee)
- ✅ Correct even with clock skew
- ❌ Requires leader round-trip (2x latency)
- ❌ Complex (lease mechanism needed)

---

## Decision

**Option A: Simple Quorum Read** - replaces strong reads entirely.

**Why:**
1. **Strictly better than strong:** Quorum verifies across majority (not one replica), survives slow/partitioned nodes
2. **Same or better latency:** Tail latency of 2nd-fastest replica (not slowest), typical ~30ms vs strong's ~50ms
3. **New constraint:** Parallel coordination (fan-out + wait) - haven't built this yet
4. **Completes quorum story:** Write durability (026) + read verification = full guarantee
5. **Quorum intersection property:** Write quorum (2/3) overlaps read quorum (2/3) → guaranteed to see acknowledged writes

**Strong reads become obsolete:** If you care enough about correctness to wait for a replica, you should wait for majority consensus instead.

**Implementation plan:**
1. Protocol: Add Quorum flag to Request (like we did for Strong/Quorum writes)
2. Client: When `--quorum` flag set, connect to ALL replicas in parallel
3. Client: Fan out GET, collect responses with (value, seq)
4. Client: Wait for majority (e.g., 2 of 3)
5. Client: Pick response with highest seq number
6. Timeout: 50ms (if majority doesn't respond, fail or escalate)
7. **Deprecate --strong flag:** Remove or alias to --quorum

**Measured tradeoff we'll demonstrate:**
- Eventual: <1ms (serve local, no guarantees)
- Quorum: ~15-60ms (majority consensus, R+W>N guarantee)

**Next step:** Add replica discovery mechanism (client needs to know all replica addresses), deprecate strong reads.
