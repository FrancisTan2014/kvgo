# Sabotage

Replica promoted during partition reconnects with incompatible sequence numbers.

## The Problem

From [015-the-split.md](015-the-split.md): when a replica is promoted while the old primary is still alive, they diverge with incompatible timelines.

When the promoted node later reconnects as a replica:
- Both nodes have valid but **incompatible** sequence numbers
- `seq=8` on the promoted node is a **different write** than `seq=8` on the original primary
- Without detection, partial resync would apply **wrong writes** in wrong order → **silent data corruption**

## Sabotage Scenario

```
1. Start P (primary) and R1 (replica)
2. Write baseline data, confirm replication works
3. Promote R1 → now TWO primaries exist (split brain)
4. Write key_p=100 to P, write key_r=200 to R1
5. Reconnect R1 to P via `replicaof`
6. Without epoch detection: R1 would try partial resync, mixing incompatible sequences
```

## Design: Epoch Detection

**Core constraint:** Sequence numbers only work within a **single timeline**.

When a node becomes primary, it starts a **new timeline** with a unique ID (replid/epoch).

### Mechanism

1. **On promote:**
   - Generate new `replid` (40-char hex, 20 random bytes)
   - Reset `seq = 0`
   - Persist to `replication.meta`

2. **On replica connect (`OpReplicate`):**
   - Replica sends both `replid` and `lastSeq`
   - Primary checks: "Is this **my** replid?"
     - If yes + backlog contains lastSeq → partial resync
     - If no → **log error** + force full resync

3. **Full resync:**
   - Primary sends `StatusFullResync` with its `replid`
   - Replica clears DB and adopts primary's replid
   - Primary sends entire dataset

### Why This Works

The replid acts as a **timeline discriminator**:
- Same replid → same timeline → sequences are compatible
- Different replid → different timeline → sequences are meaningless, must resync everything

This prevents:
- Applying writes from timeline A onto state from timeline B
- Silent data corruption from sequence number reuse

### Trade-Off

**What we gain:** Detection of split-brain, no silent corruption  
**What we pay:** Full resync on every timeline change (even if DBs are mostly identical)

This is the **Redis model**: detect divergence, force full resync, let operators prevent split-brain.

Alternative: **Raft/Paxos model** would prevent dual primaries via quorum, but requires coordination for every write.

## Implementation

- `Server.replid` generated on start and on promote
- `promote()` calls `generateReplID()` + `storeState()` + `startBacklogTrimmer()`
- `handleReplicate()` checks `rc.lastReplid != s.replid` → logs ERROR + full resync
- `replication.meta` persists `replid` and `lastSeq` across restarts

## Tests

`scripts/epoch-test.ps1` verifies:
- Promote generates new replid
- Reconnect logs "incompatible replid detected"
- Full resync clears divergent data
- Replica adopts primary's timeline after resync

Run with `--debug` to see epoch logs in stderr.

## What's Next

This implements **detection**. We do NOT prevent split-brain — operators can still promote during partition.

To **prevent** split-brain, you need:
- **Option A:** Fencing tokens (external coordination service)
- **Option B:** Consensus protocol (Raft/Paxos)
- **Option C:** Accept it, treat as operator error (current choice)

Redis chose C. Most distributed databases chose B.

We've chosen C to understand **operational boundaries** before consensus complexity.
