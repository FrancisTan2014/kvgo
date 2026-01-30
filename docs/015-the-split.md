# Sabotage
We have manual failover now (013, 014). But what if you promote a replica *while the old primary is still alive*?

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

Redis uses **Option A** (via RDB snapshot transfer). We don't have snapshots yet.

For now, we observe the bug. Fix deferred to 016.
