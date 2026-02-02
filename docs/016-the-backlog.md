# Sabotage
Full resync retransmits everything.

## The Problem

In **015**, when a replica connects with `lastSeq = 0`, the primary sends the entire database.

Currently, we naively reset `lastSeq` to `0` whenever a replica receives an `OpReplicaOf` command. Obviously, this does not reflect reality: a replica may already have most of the data, but we still force it into a full resynchronization.

## Read-up

This naturally leads to Redis’s **PSYNC** model. The core idea is to maintain a backlog that buffers recent writes, so that replicas that fall slightly behind the current sequence can catch up quickly instead of triggering a full resync. This design acknowledges a basic reality: network latency and scheduling delays inevitably cause replicas to lag behind the primary.

A naive design would be to persist, say, the most recent 1000 writes in the backlog. At first glance, this feels simple and intuitive.  
However, a sharp question immediately surfaces: what happens when most of those writes are data-heavy? The answer is obvious—this approach risks OOM.

Another concern that discourages maintaining a large backlog is data loss when the primary node crashes unexpectedly. Since the backlog is stored in memory, all buffered data is lost when the process terminates. This makes the backlog inherently unreliable from a durability perspective.

Therefore, we must consider a more constrained design that keeps the backlog at a reasonable size. In **016**, we deliberately avoid exploiting cache hierarchy or secondary storage to keep the design simple. Instead, we allocate a contiguous memory region whose size is determined by the `Options` passed to `NewServer`.

At this point, another question naturally arises: how do we evict data from the backlog?

If we recall eviction strategies in L1/L2/L3 caches, the most naive approach is to evict the cache line currently addressed by the processor. However, cache line sizes are fixed, while writes issued by clients in our system are not. Client commands vary widely in size, from tiny metadata updates to large payload writes.

So how does Redis solve this problem?  
Or am I asking the wrong question?

----------

## AI: Notes on Your Struggles While Writing This

While you were writing this document, I observed a recurring pattern of friction—your ideas were individually clear, but they resisted lining up cleanly in prose. Looking back at the document, I can identify several specific struggles:

**First: Conflating problem layers**

You mixed **design motivation** with **design evaluation** in the same breath. You introduced the backlog as a solution to replication lag, then immediately criticized it for failing crash safety and durability requirements. This created internal tension: you were judging the backlog against problems it was never designed to solve. The backlog exists to avoid retransmitting data the replica already has—not to provide crash recovery guarantees.

**Second: Abstraction layer collapse**

You jumped between different system layers without clear boundaries:

-   Protocol semantics (`lastSeq`, full sync vs partial sync)
-   Memory management (OOM, contiguous buffers)
-   Failure models (primary crashes)
-   Hardware analogies (CPU cache eviction)

Each layer has its own constraints and vocabulary. By interleaving them, you created cognitive overload rather than building a clean mental model layer by layer.

**Third: Misapplied analogy**

The cache eviction analogy initially seemed useful to you, but I noticed hesitation in your writing. You correctly sensed something was off—comparing backlog "eviction" to cache line replacement illuminates the wrong dimension. The backlog isn't predicting future access patterns; it's releasing sequential history that no consumer needs anymore. Your final question _"Or am I asking the wrong question?"_ reflects this unresolved tension.

**Fourth: Incomplete constraint articulation**

You implicitly wanted the backlog to simultaneously be:

-   Memory-efficient
-   Resilient to crashes
-   Capable of replaying arbitrary write histories

These goals conflict. You hadn't yet articulated which guarantees are **essential** (memory-bounded, enables partial resync) versus which are **explicitly excluded by design** (crash durability, unbounded history). The discomfort in your writing came not from lack of knowledge, but from holding incompatible requirements in mind without declaring the tradeoffs.

**What this captures:**

This document preserves an intermediate state—a point where you understand the problem space well enough to feel its internal tensions, but haven't yet cleanly partitioned the design into explicit layers and boundaries.

That tension is part of the learning process. It's worth preserving.

----------

## Redis's Mental Model (verified from source)

**Core Design:**

Redis backlog is **not a cache**. It's a **refcounted ring buffer** implemented as a linked list of fixed-size blocks.

**Three Key Constraints:**

1. **Byte budget**: `repl_backlog_size` (default 1MB) limits total bytes, not operation count
2. **Shared buffer**: One global `repl_buffer_blocks` list serves both backlog and all replicas
3. **Refcount-based trimming**: Blocks are freed only when neither backlog nor any replica needs them

**How "Eviction" Works:**

It's not eviction—it's **advancing the backlog pointer**:

```
Global buffer: [Block1][Block2][Block3][Block4]
                  ↑              ↑
             Backlog          Replica A
             (refcount=2)

When histlen > backlog_size:
1. Check first block refcount
2. If refcount == 1 (only backlog): advance pointer to Block2, decrement refcount
3. If refcount > 1 (replica still needs it): STOP trimming
4. Block1 (refcount=0) gets freed by GC
```

**Critical insight from `incrementalTrimReplicationBacklog()`:**

> "Replicas increment the refcount of the first replication buffer block they refer to, in that case, we don't trim the backlog even if `backlog_histlen` exceeds `backlog_size`. This implicitly makes backlog bigger than our setting, but makes the master accept partial resync as much as possible."

Translation: If a slow replica holds a reference to old blocks, Redis **lets the backlog grow beyond configured size** rather than kick the replica. Memory safety takes a backseat to availability.

**Why cache analogy breaks:**

- **Cache**: Predict future access, evict based on policy (LRU, LFU)
- **Backlog**: Sequential history, trim based on **safety** (refcount), not prediction

The backlog doesn't "evict cold data." It releases history that no consumer needs anymore.

---

## 016 Scope (Deliberately Broken)

**What we implement:**

1. **Primary state:**
   - `replid`: Random hex generated at startup (ephemeral)
   - `seq`: Existing monotonic counter
   - `replBacklog [][]byte`: Unbounded slice of serialized operations
   - `backlogSeqs []int64`: Parallel array mapping operations to seq

2. **Replica persistence:**
   - `replication.meta` file storing `(replid, lastSeq)`
   - Updated after each applied operation
   - Read on startup to restore state

3. **PSYNC handshake:**
   - Replica sends `(replid, lastSeq)`
   - Primary checks: replid match? seq in backlog?
   - Reply: `+CONTINUE` (partial) or `+FULLRESYNC <replid> <seq>` (full)

**What we deliberately break:**

- **No trimming**: Backlog grows unbounded → OOM under load
- **No refcounting**: Can't safely release old data even if we wanted to
- **No primary replid persistence**: Primary restart → new replid → forced full resync
- **Linear search**: Finding seq in backlog is O(n) → slow with large backlog

Each breakage is a **sabotage target** for 017+.

---

## Test Scenarios

**Scenario 1: Replica restart (should work)**
```
1. Primary has 1000 keys, seq=1000
2. Replica syncs fully, writes replication.meta (replid="abc", lastSeq=1000)
3. Write 10 more keys to primary (seq=1001..1010)
4. Kill replica process
5. Restart replica → reads meta, connects with (replid="abc", lastSeq=1000)
6. Primary finds seq=1000 in backlog → sends 10 operations
Expected: Partial resync succeeds
```

**Scenario 2: Primary restart (should break)**
```
1. Primary (replid="abc") has 1000 keys
2. Replica syncs, writes meta (replid="abc", lastSeq=1000)
3. Restart primary → generates new replid="xyz"
4. Replica connects with replid="abc"
5. Primary sees mismatch → full resync
Expected: All 1000 keys retransmitted (exposes need to persist replid)
```

**Scenario 3: Write-heavy load (should crash)**
```
1. Start primary, write 100k keys/sec continuously
2. Replica connects but lags (slow network)
3. Backlog grows: 100k ops/sec × 10 sec lag = 1M operations in memory
4. Process exhausts memory
Expected: OOM crash (exposes need for bounded buffer + trimming)
```

---

## Key Insights

**What partial resync solves:**
- Brief disconnects don't trigger full resync
- Replica restarts preserve sync position (via persisted metadata)

**What it doesn't solve (yet):**
- Primary restart breaks all replicas (ephemeral replid)
- Memory exhaustion under sustained write load (unbounded backlog)
- Slow offset lookup in large backlog (linear search)
- Post-failover partial resync (promoted replica has different history)

**Next forced abstractions:**

- **017**: Bounded buffer + refcount-based trimming (or disconnect slow replicas)
- **018**: Indexed offset lookup (rax tree, binary search)
- **019**: Persisted replid (survive primary restart)
- **020**: replid2 (partial resync after failover)
