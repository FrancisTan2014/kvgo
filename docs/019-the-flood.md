# Sabotage

Sustained writes exhaust primary memory via unbounded replication backlog.

## The Problem

The replication backlog (`replBacklog`) grows without bound. Every write appends to the slice; nothing ever trims it. Under sustained write load, the primary will OOM.

Secondary issues:
- `getSeqIndex()` does O(n) linear scan — performance degrades as backlog grows
- No mechanism to disconnect slow replicas that fall too far behind

## Sabotage Scenario

```
1. Start primary
2. Connect replica (optional — backlog grows regardless)
3. Write 100k keys/sec continuously
4. Observe memory growth: 100k ops/sec × 60 sec = 6M operations buffered
5. Primary exhausts memory → OOM crash
```

## Design

Redis trims **inline on write** (when adding a new block) and **never disconnects slow replicas** — it lets the backlog exceed the limit to preserve partial resync eligibility.

Our design makes the opposite trade-off: **memory bounds over partial resync**.

Key decisions:
1. **Periodic trimming** (100ms default) — not inline on write, to avoid hot-path overhead
2. **Hysteresis threshold** (2x limit) — don't trim until we exceed 2x the limit, then trim down to 1x. Reduces churn.
3. **Byte-based limit** (16MB default) — not entry count, since large writes would otherwise exhaust memory with few entries

```
Constants:
  DefaultBacklogSizeLimit = 16MB
  TrimRatioThreshold = 2  // Trim when size > 2x limit

On write:
  append to backlog
  backlogSize += entrySize

Background trimmer (every 100ms):
  if backlogSize > limit * TrimRatioThreshold:
    while backlogSize > limit:
      pop oldest entry
      backlogSize -= entry.size
```

## Implementation

- `container/list` for O(1) head removal
- `sync.RWMutex` protects list operations
- `atomic.Int64` for lock-free size reads
- Trimmer goroutine derives context from server; cancelled on shutdown or role change

## Tests

Unit tests in `server/backlog_test.go` verify:
- Trimmer fires when exceeding 2x threshold
- Trimmer does not fire below threshold
- `existsInBacklog()` range checking
- `forwardBacklog()` iteration and trimmed-seq handling