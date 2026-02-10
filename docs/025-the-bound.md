# Sabotage
Replica 1 hour behind. Should it refuse reads?

## Current behavior
Replicas serve reads regardless of lag. Could be 50 ops behind or 60,000 ops behind - no rejection.

Without bounds, clients get arbitrarily stale data.

**Different from Episode 024:**
- 024: Client-specific (read-your-writes for MY writes)  
- 025: Cluster-wide health (when is replica "too stale"?)

## Real systems
- **Redis:** `min-replicas-max-lag` stops primary writes (not replica reads)
- **MongoDB:** `maxStalenessSeconds` rejects stale replicas  
- **Cassandra:** No bounds, client controls via consistency level
- **PostgreSQL:** `max_standby_*_delay` cancels queries on lag


## Design space

**Option A: Time-based**
```go
if time.Since(lastHeartbeat) > 5*time.Second {
    return StatusReplicaTooStale
}
```
- ✅ Detects partition immediately
- ❌ Clock skew causes false positives
- ❌ Heartbeat delay ≠ data staleness

**Option B: Sequence-based**
```go
if primarySeq - replicaSeq > 1000 {
    return StatusReplicaTooStale
}
```
- ✅ Measures actual data lag
- ✅ No clock skew issues
- ❌ During partition, `primarySeq` frozen (stale knowledge)
- ❌ Hard to tune (1000 ops = what latency?)

**Option C: Hybrid**
```go
if time.Since(lastHeartbeat) > 5*time.Second || 
   primarySeq - replicaSeq > 1000 {
    return StatusReplicaTooStale
}
```
- ✅ Catches both partition (time) and backlog (seq)
- ✅ Tunable per scenario
- ❌ Two knobs = operator complexity

**Option D: No bound (current)**
- Maximum availability, zero safety

## Decision

**Hybrid (Option C)** - defensive depth catches both failure modes:
- Time threshold → partition detection
- Sequence threshold → backlog limit  
- Together → covers different failures

Configuration:
```go
MaxHeartbeatAge: 5s      // partition detection
MaxSequenceLag:  1000    // backlog limit
```

Client behavior on `StatusReplicaTooStale`: retry another replica or escalate to primary.
