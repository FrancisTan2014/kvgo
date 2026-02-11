# kv-go Progress Tracker

**Last updated:** February 11, 2026

**Purpose:** Living document tracking what's implemented, what's next, and open threads. Read this before proposing new work or when context feels fuzzy.

**⚠️ REQUIRED READING FOR ALL AGENTS:** Read this file FIRST at the start of every new conversation. This is your source of truth for project state.

---

## Timeline Context

**Project started:** January 21, 2026 (first commit)  
**Current date:** Check system date  
**Episode count:** 27 episodes complete

**⚠️ CRITICAL FOR AGENTS:** Calculate timeline phase from start date:
- **Months 1-4 (Build):** Deep understanding + features from open threads
  - Allow 15-30 min struggle if it teaches constraints
  - Propose features, not polish
- **Month 5 (Polish):** Benchmarks, architecture docs, blog posts
  - Allow 10-15 min struggle max
  - Focus on portfolio artifacts
- **Month 6 (Ship):** Job applications, interview prep

**Formula:** `weeks_elapsed = (current_date - 2026-01-21) / 7`
- Weeks 0-17: Build phase (Months 1-4)
- Weeks 18-21: Polish phase (Month 5)
- Weeks 22-26: Ship phase (Month 6)

**Episode count ≠ timeline phase.** Always compute from dates.

---

## Current Status: Episode 027 Complete

**What works now:**
- Single primary + N replicas replication topology
- Write routing: StatusReadOnly redirects client from replica to primary (023)
- Read anywhere: Replicas serve reads with optional consistency guarantees
- **Session consistency (024):** Read-your-writes via version tokens (REMOVED in 027)
- **Staleness bounds (025):** Hybrid time+lag bounds reject too-stale replicas
- **Quorum writes (026):** Optional quorum commit with ACK/NACK protocol via `--quorum` flag
- **Quorum reads (027):** Optional quorum reads with server-side fan-out via `--quorum` flag
- Split-brain recovery: Full resync when replica reconnects after partition (015)
- Crash recovery: WAL replay, value file compaction (021)

**Consistency modes:**
- **Default (eventual):** GET serves immediately, may be stale (matches Redis READONLY)
- **Quorum (--quorum):** GET fans out to majority, returns highest seq (linearizable)
- ~~Strong (--strong): Removed in 027, replaced by quorum (strictly better guarantee)~~

**Write durability modes:**
- **Default (async):** PUT returns after WAL write, async replication (matches Redis)
- **Quorum (--quorum):** PUT waits for majority ACKs before returning (stronger durability)

**Recent work (027-the-quorum-read):**
- Protocol: Removed WaitForSeq field (30→22 byte header), added FlagRequireQuorum
- Server: doQuorumGet with local read first, fan-out to majority, CAS loop for seq+val
- Thread-safe helpers: addReachableNode/removeReachableNode/snapshot for safe iteration
- Client: replaced `--strong` with `--quorum` flag (breaking change)
- Tests: 9 unit tests (response validation, thread safety, staleness)
- Removed strong reads entirely: quorum reads strictly better (linearizable vs read-your-writes)

**Also complete (026-the-quorum-write):**
- ACK/NACK protocol for write durability
- Quorum writes with majority fan-out

---

## Open Threads (From Episode 022)

### 1. Session Guarantees ✅ COMPLETE (Episode 024)
**Status:** Implemented with version token approach (optional WaitForSeq flag)

**Implementation:**
- Default: eventual consistency (fast path, benchmark vs Redis)
- Opt-in: strong consistency via `--strong` flag (read-your-writes)
- Failure mode: timeout → redirect to primary
- Tests: all passing (strong read, eventual read, timeout)

See `docs/024-the-session.md` for full design and implementation details.

---

### 2. Staleness Bounds ✅ COMPLETE (Episode 025)
**Status:** Implemented with hybrid approach (time + lag bounds)

**Implementation:**
- Time threshold: reject if >5s since last heartbeat (partition detection)
- Lag threshold: reject if >1000 ops behind primary (backlog detection)
- Returns: StatusReplicaTooStale (client redirects to primary or retries)

See `docs/025-the-bound.md` for design rationale.

---

### 3. Client Contract
**Question:** Do clients know about topology? How do they discover replicas?

**Current behavior:**
- Clients hardcode addresses (kv-cli connects to one address)
- No service discovery
- No health checking

**Options:**
- Load balancer fronts cluster (clients see one address)
- Client-side awareness (client knows primary + replica addresses)
- DNS-based discovery

**Status:** Not implemented. Design not chosen.

---

## Architecture Decisions Made

### Replication Model: Primary-Replica (Single Writer)
**Why:** Simple, matches Redis. Avoids multi-master conflict resolution.
**Tradeoff:** Primary is SPOF for writes. Failover requires promotion.

### Write Routing: Redirect (StatusReadOnly)
**Why:** Simpler than proxy, no replica SPOF. Client retries are acceptable.
**Tradeoff:** Extra network hop vs proxy. Breaking protocol change.

### Consistency Model: Eventual
**Why:** Async replication, replicas serve reads without coordination.
**Tradeoff:** Stale reads possible. No linearizability.

### Split-Brain Recovery: Full Resync
**Why:** Diverged histories are incompatible (seq numbers overlap).
**Tradeoff:** Expensive for large DBs. Discards replica's writes.

### Session Consistency: Optional Version Token (024, IN PROGRESS)
**Why:** Balances learning + portfolio + benchmark requirements.
- Default (no WaitForSeq): eventual consistency, fast, benchmark vs Redis
- Strong (WaitForSeq): read-your-writes guarantee, adds latency
**Tradeoff:** 
- ✅ Teaches thread synchronization + timeouts
- ✅ Produces benchmark artifact (eventual vs strong read latency)
- ✅ Optional (preserves fast path for Redis comparison)
- ❌ Blocking mechanism complexity
- ❌ Client state tracking

---

## Implementation Roadmap

### Phase 1: Local Correctness ✅
- [x] 001-003: WAL, durability, AOF
- [x] 004-006: Flush points, compaction
- [x] 021: CLEANUP command (value file compaction)

### Phase 2: Concurrency ✅
- [x] 002: Locks, group commit
- [x] Sharding (implicit via key-value separation)

### Phase 3: Replication ✅
- [x] 009: Primary-replica topology
- [x] 010: Consistency models (conceptual)
- [x] 015: Split-brain recovery (full resync)
- [x] 020: Epoch-based failover
- [x] 023: Write routing (StatusReadOnly redirect)
- [x] 024: Session guarantees (version token) - REMOVED in 027
- [x] 025: Staleness bounds (hybrid time+lag)
- [x] 026: Quorum writes (ACK/NACK protocol)
- [x] 027: Quorum reads (server-side fan-out)

### Phase 4: Failure Modes & Observability
- [x] 011: Latency measurement
- [x] 012: Crash recovery
- [x] 013: Network outage handling
- [x] 014: Failover mechanics
- [x] 016: Replication backlog
- [x] 018: Orphan state (promoted replica cleanup)
- [x] 019: Flood protection

### Phase 5: Performance & Polish (Month 5)
- [ ] Benchmark suite vs Redis (80% target)
- [ ] Architecture documentation with diagrams
- [ ] Blog posts explaining design decisions
- [ ] README polish

---

## Episode 025 - The Bound ✅ COMPLETE

**Constraint felt:** Replica 1 hour behind still serves reads → arbitrarily stale data

**Question:** When should replica say "I'm too stale, ask someone else"?

**Approach implemented:** Hybrid bounds (time + sequence lag)
- Time-based: reject if >5s since last heartbeat (detects partition)
- Lag-based: reject if >1000 ops behind (detects backlog)
- Returns: StatusReplicaTooStale → client redirects

**Implementation:**
1. ✅ Server: `isStaleness()` checks both thresholds before serving reads
2. ✅ Config: ReplicaStaleHeartbeat and ReplicaStaleLag options
3. ✅ Tests: staleness detection with time/lag scenarios

**Achieved outcomes:**
- Completes replica read story: fast → acceptable → too stale → reject
- Portfolio: "implemented both time and logical clock bounds"
- Interview: "defensive depth - catches partition AND backlog failures"

**Documentation:** `docs/025-the-bound.md` (hybrid approach rationale)

---

## Episode 026 - The Quorum Write ✅ COMPLETE

**Constraint felt:** Async writes (default) are fast but data lost if primary crashes before replication

**Question:** How do you make writes durable without always paying quorum latency?

**Approach implemented:** Optional quorum flag with ACK/NACK protocol
- Default `PUT` → async replication (fast, benchmark vs Redis)
- Quorum `PUT --quorum` → wait for majority ACKs (durable, slower)

**Implementation:**
1. ✅ Protocol: Added CmdAck, CmdNack, Quorum flag to requests
2. ✅ Server: Per-request quorum state, parallel ACK collection, per-replica dedup
3. ✅ Dispatcher: Unified request handling (replicas send ACK/NACK/PONG as requests)
4. ✅ Client: `--quorum` flag for PUT command
5. ✅ Tests: 10 new tests (handlers, dispatcher, replication)

**Achieved outcomes:**
- Benchmark-ready: async vs quorum write latency comparison
- Portfolio artifact: demonstrates understanding of consensus tradeoffs
- Interview prep: "configurable durability, measured cost"

**Documentation:** `docs/026-the-quorum-write.md` (design + implementation)

---

## Episode 027 - The Quorum Read ✅ COMPLETE

**Constraint felt:** Strong reads (WaitForSeq) provide read-your-writes but not linearizable (can read stale if replica is behind). Quorum reads provide stronger guarantee.

**Question:** Should we add quorum reads or keep strong reads? Can we have both?

**Approach implemented:** Replace strong reads with quorum reads (strictly better)
- Removed WaitForSeq from protocol entirely (breaking change: 30→22 byte header)
- Server-side fan-out: local read first, then fan-out to majority, pick highest seq
- Replicas accept both StatusOK and StatusNotFound as valid responses
- Thread-safe reachableNodes helpers prevent mutex forgetting

**Implementation:**
1. ✅ Protocol: Removed WaitForSeq field, FlagRequireQuorum for quorum reads
2. ✅ Server: doQuorumGet with CAS loop for atomic seq+val update
3. ✅ Helpers: addReachableNode/removeReachableNode/clearReachableNodes/getReachableNodesSnapshot
4. ✅ Staleness: Restored staleness checks (runs before ANY read)
5. ✅ Client: Replaced `--strong` with `--quorum` flag
6. ✅ Tests: 9 unit tests (response types, thread safety, staleness)

**Achieved outcomes:**
- Linearizable reads: quorum intersection (R+W>N) ensures seeing latest write
- Simplified protocol: one consistency model removed (eventual or quorum)
- Portfolio: "implemented quorum consensus for reads and writes"
- Interview: "knew when to remove complexity - quorum strictly dominates strong reads"

**Documentation:** `docs/027-the-quorum-read.md` (3 options evaluated, chose Simple Quorum)

**Critical bugs fixed during review:**
1. Missing local read (only fanned out to replicas)
2. Race condition on val update (TOCTOU bug)
3. Wrong routing in handleGet (prevented primaries from quorum reads)
4. reachableNodes not populated when replicas connect

---

## Episode 024 - The Session ✅ COMPLETE (REMOVED in Episode 027)

**Constraint felt:** Client writes to primary (seq=500), reads from replica (lastApplied=485) → sees NotFound

**Question:** How do you guarantee read-your-writes without destroying read scaling?

**Approach implemented:** Version token (Option A) with optional flag
- Default `GET` → eventual (fast, benchmark vs Redis)
- Strong `GET --strong` → blocks until caught up (read-your-writes)

**Implementation:**
1. ✅ Protocol: Added `Seq` to responses, `WaitForSeq` to requests (uniform header)
2. ✅ Server: Capped exponential backoff (1→2→4→8→10ms), timeout redirect
3. ✅ Client: Session tracking, `--strong` flag parsing
4. ✅ Tests: 3 comprehensive tests (all passing)

**Achieved outcomes:**
- Benchmark-ready: eventual reads vs Redis (Option A clean comparison)
- Portfolio artifact: measurable tradeoff (eventual: <1ms, strong: ~10-50ms)
- Interview prep: "implemented both, here's the data"

**Documentation:** `docs/024-the-session.md` (design + full implementation)

---

## Next Steps (Open)

**Possible directions:**

1. **Staleness bounds** (Thread #2 above)
   - Reject reads if replica > N seconds behind
   - Requires: heartbeat tracking, time-based threshold

2. **Quorum reads/writes** (stronger than read-your-writes)
   - Read from multiple replicas, wait for majority
   - Requires: parallel fan-out, vote counting

3. **Month 5: Polish for portfolio**
   - Benchmark harness (Redis comparison numbers)
   - README improvements (architecture diagram, getting started)
   - Blog posts (design decisions, tradeoffs)

**Recommendation:** Continue building (Month 1-4 focus) OR move to Month 5-6 polish depending on timeline pressure.

---

## Update Instructions for Agents

**When to update this file:**
1. After completing an episode (move from "open" to implemented)
2. When making architecture decisions (add to "Architecture Decisions Made")
3. When discovering new open threads (add to "Open Threads")
4. When starting new work (update "Next:" section)

**How to update:**
- Don't rewrite entire file
- Update specific sections that changed
- Keep "Last updated" date current
- Keep "Current Status" accurate

**When to read this file:**
1. Before proposing next episode (check open threads, don't duplicate)
2. When context feels fuzzy (re-ground in what's implemented)
3. When user asks "where are we?" (read and summarize)
