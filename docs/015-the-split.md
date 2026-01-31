# Sabotage
What if you promote a replica *while the old primary is still alive*?

## Setup

**Scenario:**
1. P (primary) and R1 (replica) are running.
2. Network partition: P and R1 can't talk to each other, but both are alive.
3. You run `promote` on R1.
4. Now there are **two primaries**.

**Question 1:** What happens if you write different keys to each side?

**Question 2:** What happens when you heal the partition (run `replicaof` to reconnect R1 to P)?

Run `split-brain-test.ps1` to find out.

---

## Observations

### Expected
- During the split: P and R1 diverge. Each has writes the other doesn't.
- After healing: R1 reconnects to P and starts following it again.

### Actual
After running the test:
- R1 reconnects to P.
- **R1 did not receive P's new writes** (`key_p` is missing).
- **R1 kept its own writes** (`key_r` still exists).

**Why?**

When R1 reconnects, it sends `OpReplicate` with `lastSeq`. But:
- While R1 was a Primary, it issued its own sequence numbers.
- P issued different sequence numbers for the same period.
- When R1 says "I have seq 5", P thinks "OK, start from 6", but P's seq 6 is a **different write** than R1's seq 6.

**The sequences are incompatible.**

---

## The Question

When a node switches from Primary → Replica (via `replicaof`), what should happen to its local state?

**Option A:** Reset `lastSeq` to 0. R1 requests everything from P (full resync).  
**Option B:** Keep `lastSeq`. Assume the gap will "fill in" (wrong assumption, but simple).  
**Option C:** Add an "epoch" number to detect incompatible histories.

Redis uses **Option A** (via RDB snapshot transfer). We don't have snapshots yet, but we can do the same logic:

---

## The Fix

When a node receives `OpReplicaOf`, it must **discard its entire history** and rebuild from the new primary.

**Replica Side:**
1. Close connection to old primary (if any).
2. **Clear the entire database** (drop all keys).
3. **Reset `lastSeq` to 0**.
4. Connect to new primary with seq=0.

**Primary Side:**
When a replica connects with `lastSeq=0`, the primary must send the **full DB state**:
1. Detect `lastSeq=0` in `OpReplicate` handshake.
2. Iterate all keys in the database.
3. Send each key-value pair as an `OpPut` with the current sequence.
4. After snapshot completes, continue forwarding new writes.

**Result:**
- R1 loses its stale writes (`key_r=200`).
- R1 receives full snapshot from P (including `key_p=100` and `baseline=sync`).
- Clean slate, no ghost data.

**Trade-off:**
- Full snapshot is expensive for large DBs (blocks the primary during iteration).
