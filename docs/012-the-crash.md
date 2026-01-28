# Sabotage
What if a replica dies mid-flight?

## The experiment

```powershell
.\scripts\crash-test.ps1
```

Kill replica 500ms into a 50,000-write benchmark.

## What we observed

**Benchmark results:**
- 50,000 writes completed
- 0 errors
- ~5,000 req/s throughput
- Primary kept running, no crash, no pause

**Replica received before death:**
- 33 writes (seq 1-33)
- Killed at 09:35:16.144

**Primary behavior:**
- No log entry about replica disconnect
- No awareness that redundancy was lost
- Continued accepting writes as if nothing happened

**The gap:**
- Primary: seq 1 → 50,000
- Replica: seq 1 → 33
- Missed: ~49,967 writes (99.9%)

## What breaks

Nothing "breaks" in the immediate sense. That's the problem.

1. **Silent failure** - Primary doesn't know replica died
2. **Lost redundancy** - You think you have backup, you don't
3. **Replication gap** - If primary dies now, 49,967 writes are gone
4. **No alerting** - Ops has no idea

## What to do

Current state: fire-and-forget async replication. Primary only detects replica death when a write fails. If traffic stops, dead replicas go unnoticed.

**Design: Heartbeat on idle**

Add a keepalive to `replicaWriter()`:

1. Use a ticker (e.g., 5 seconds)
2. On tick, if no write since last tick, send a PING
3. If PING write fails → connection broken → remove replica, log it

```
replicaWriter loop:
  select {
    case payload := <-sendCh:
      write(payload)
    case <-ticker:
      if idle {
        write(PING)  // empty frame or new OpPing
      }
  }
```

**What this gives us:**
- Detection within 5 seconds of replica death
- No change to write path (still async)
- Ops sees "replica X disconnected" in logs

**What it doesn't give us:**
- Write durability guarantees (still fire-and-forget)
- Automatic failover
- Consensus (that's way later)

This is bounded: one struct field (`lastWrite`), one ticker, ~20 lines of code. After this, silent failure becomes loud failure.
