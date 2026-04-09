# 037k — The Partition

037j gave us the bridge — HTTP API, Jepsen scaffold, Docker cluster. The basic workload passes against a healthy cluster. But a healthy cluster is the uninteresting case. We know reads go straight to the local state machine with no leader check. We've said since 037a that a partitioned leader will serve stale data. We have no evidence. This episode adds the nemesis and captures the failure.

## Boundary

Add a network partition nemesis to the Jepsen test suite. A single command runs the partition workload against a live cluster and produces `{:valid? false}` — proving that kv-go serves stale reads under partition.

In scope:
- `partition.clj` workload — reads and writes under network partitions using Jepsen's `nemesis/partition-random-halves`
- Nemesis integration in the test harness — `:nemesis` on the test map, nemesis operations interleaved with client operations in the generator
- `iptables`-based partitioning via Jepsen's built-in partition nemesis (already available in the Docker node images)
- Workload registration in `kvgo.clj` — selectable via `lein run test -w partition`
- Evidence: the checker output, timeline HTML, and performance graphs that show the stale read

Deferred:
- ReadIndex fix (037l) — the fix for the failure this episode proves
- Partition-aware client (retry on a different node when the current node returns errors)
- CAS operations (compare-and-swap workload for stronger linearizability checks)

Out of scope:
- Process kill nemesis — separate failure mode, already unit-tested in 037g
- Clock skew nemesis — Docker containers lack real clocks
- Custom partition strategies (isolate leader specifically) — random halves is sufficient to trigger the bug reliably

## Design

### Why random halves

`nemesis/partition-random-halves` splits the 5-node cluster into two groups at random. With 5 nodes, the split is always 2 vs 3. The minority side might contain the old leader — and that's the case that produces stale reads. Random selection means some test runs partition the leader, some don't. Over a 30-second test with multiple partition/heal cycles, the probability of never partitioning the leader is negligible.

A targeted "always isolate the leader" nemesis would be more deterministic but masks a real failure mode: what if the partition doesn't include the leader? The system should still be correct. Random halves tests both cases.

### Generator structure

The generator interleaves client operations (reads and writes) with nemesis operations (partition and heal):

```clj
(gen/nemesis
  (cycle [(gen/sleep 5)
          {:type :info, :f :start}   ;; partition
          (gen/sleep 5)
          {:type :info, :f :stop}])  ;; heal
  (gen/mix [r w]))
```

Every 5 seconds, the nemesis toggles: partition → heal → partition → heal. Client operations run continuously. During partition, the minority-side leader serves stale reads; during heal, the cluster converges. The checker sees the full interleaved history and flags any linearizability violation.

### Why the test fails

1. Leader L is on node n1. Client writes `jepsen = 3` through L. Committed, applied, acknowledged.
2. Nemesis partitions the cluster: {n1, n2} vs {n3, n4, n5}.
3. n3, n4, n5 elect a new leader L'. Client writes `jepsen = 4` through L'. Committed on the majority side.
4. Client reads `jepsen` from n1 (old leader). n1 still thinks it's the leader — no leader check on reads, no ReadIndex. Returns `3`.
5. The checker sees: write 4 completed, then read returned 3. Not linearizable.

The HTTP GET path in `httpGet` reads directly from `s.sm.Get(key)` — the local state machine. There is no leader verification, no quorum check, no ReadIndex. This is the deliberate gap left by 037j's design: "reads are intentionally unsafe."

### What the timeline shows

The timeline HTML will show two threads of operations. On one thread, a `:write 4 :ok` completes. On another thread, a `:read 3 :ok` starts after the write returns. The linearizability checker marks this pair as the conflict. The visualization makes the stale read visible — a concrete artifact, not a theoretical concern.

### Workload registration

`partition.clj` follows the same pattern as `basic.clj` — a `workload` function returning `{:nemesis :checker :generator}`. The main namespace registers it:

```clj
(def workloads
  {"basic"     basic/workload
   "partition" partition/workload})
```

Run with: `lein run test -w partition`.

## Minimum tests

**Invariant:** network partitions cause stale reads in the current implementation — the linearizability checker detects the violation.

1. **Partition workload produces `{:valid? false}`** — the full Jepsen test with partition nemesis reports a linearizability violation. This is the primary deliverable: evidence that stale reads exist under partition.
2. **Timeline shows stale read** — the timeline HTML contains at least one pair where a read returns a value that was already overwritten by a committed write. Visual evidence, not just a boolean.
3. **Heal restores convergence** — after the partition heals, reads and writes resume normally. The nemesis cycles partition/heal multiple times; the system recovers each time. Failures only occur during or immediately after partition.
4. **Basic workload still passes** — no regression. `lein run test -w basic` continues to report `{:valid? true}`.

## Open threads

1. **ReadIndex** — the fix for this episode's failure. Leader checks quorum liveness before serving a read. 037l implements this and re-runs the partition workload to get `{:valid? true}`.
2. **Client retry** — the Jepsen client currently pins to one node. If that node is on the minority side, all operations fail or return stale data. A retry-on-different-node strategy would make the test more realistic but isn't needed to prove the violation.
3. **CheckQuorum** — the leader could step down proactively if it stops receiving heartbeat responses from a majority. Complementary to ReadIndex but separate — ReadIndex is per-read, CheckQuorum is leader-level.
