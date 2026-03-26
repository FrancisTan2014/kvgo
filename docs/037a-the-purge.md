# 037a — The Purge

036 is complete. A PUT proposed on the leader reaches committed state in all followers' state machines — tick-driven election, TCP transport, log replication, and the apply loop, all wired end-to-end. The Raft core is real. It elects, replicates, and applies.

But the server still runs the old system alongside it. Phase 1 elects its own leader through `runElection`. Phase 1 replicates through `forwardToReplicas`. Phase 1 heartbeats through `heartbeatLoop`. Two election systems, two replication systems, two heartbeat systems, two sources of term truth — running in parallel inside the same process.

This is worse than either system alone. A split-brain isn't two clusters disagreeing — it's one process disagreeing with itself. The Raft leader and the Phase 1 leader can diverge. A write can succeed through Raft and fail through Phase 1's quorum check, or the reverse. No test can prove correctness when two authorities own the same state.

036 proved Raft can own the write path. 037 removes everything that competes with it.

## Boundary

037 purges all Phase 1 code first, then reimplements what's needed on a clean Raft-only architecture. When 037 is complete, kv-go is entirely an etcd-like system: Raft owns election, replication, and state mutation. The KV store is a state machine that the log drives.

**Series invariant:** every request path that touches state flows through Raft; everything else is stateless.

In scope:
- Purge all Phase 1 code in one episode — election, replication, heartbeat, fencing, backlog, peer management, quorum machinery, coordination handlers, and every server field that belongs to them
- Purge Phase 1 integration tests and rebuild for Raft — obsolete scripts deleted, surviving scenarios rewritten
- Build Raft-native equivalents for capabilities the core lacks — PreVote, CheckQuorum, leadership transfer
- Reimplement upper-layer features on the clean architecture — GET, DELETE, write redirect, staleness bounds, session consistency, HEALTH, `engine.DB` as `StateMachine`
- Reimplement infrastructure — bootstrap/restart, leader routing, kv-server startup flags, kv-cli or a real client layer, engine compaction integrated with Raft snapshots

Deferred:
- Linearizable reads (`ReadIndex` protocol) — future episode
- Cluster membership changes (conf-change through Raft) — future episode

Out of scope:
- Backward compatibility with Phase 1 protocol (there are no clients to break)
- Preserving Phase 1 code paths "just in case"

## Design

### The seam between 036 and 037

036 asked: can Raft own the write path? The answer is yes — a PUT enters the log, gets majority agreement over TCP, and only then applies to the state machine.

037 asks: can Raft own *everything*? 036 built a foundation. 037 removes the scaffolding.

### Why purge first, reimplement second

A conservative migration would remove Phase 1 piece by piece, keeping the server functional after every step — dependency ordering, dual-path management, careful sequencing. That's production thinking. There are no clients to break.

Purging first is better for three reasons. First, a clean slate forces real design. Reimplementing GET on a gutted Raft-only server means thinking about how reads work in an etcd-like system, not just swapping `db` for `sm`. Second, no hybrid navigation. Every episode after the purge builds on Raft alone — no working around Phase 1 code that's about to die. Third, the series invariant becomes trivially true after the purge. Phase 1 is gone, so every stateful path flows through Raft because there's nothing else. Each reimplementation episode adds surface area while maintaining the invariant.

The server won't fully work after the purge episode. Only PUT through Raft survives. That's the point — subsequent episodes reimplement each command against the architecture it deserves.

### What Raft truly subsumes

These Phase 1 features disappear because the Raft algorithm already provides the same guarantee at a lower layer. They need no replacement.

- **Quorum writes** ([026-the-quorum-write](026-the-quorum-write.md)) — Raft committed *is* a quorum write. The explicit ACK/NACK fan-out was a workaround for local-write-first ordering.
- **Quorum reads** ([027-the-quorum-read](027-the-quorum-read.md)) — peer-based parallel reads with highest-seq-wins. Real linearizable reads need `ReadIndex`, which is future work. The Phase 1 mechanism is gone.
- **Phase 1 heartbeat** ([012-the-crash](012-the-crash.md)) — `heartbeatLoop` + `PING` carried term and seq to replicas. Raft's `MsgHeartbeat` does the same thing, integrated with the log and commit index ([036t-the-tick](036t-the-tick.md)).
- **Phase 1 election** ([031-the-election](031-the-election.md)) — `runElection` + `VOTE_REQUEST`. Raft elects through `MsgVote` ([036h-the-election](036h-the-election.md)).
- **Partial resync / backlog** ([016-the-backlog](016-the-backlog.md), [019-the-flood](019-the-flood.md)) — the backlog buffered recent writes for reconnecting replicas. Raft has snapshot + log ([036e-the-compaction](036e-the-compaction.md), [036k-the-snapshot-transfer](036k-the-snapshot-transfer.md)).
- **Split-brain detection** ([015-the-split](015-the-split.md), [020-the-epoch](020-the-epoch.md)) — `replid` + `seq` detected divergent histories. Raft's single-leader-per-term and log matching property prevent divergence structurally ([036h-the-election](036h-the-election.md), [036i-the-freshness](036i-the-freshness.md)).
- **metafile persistence** — `term`, `votedFor`, `replid`, `lastSeq` in a separate file. Raft's `HardState` in `raftStorage` is the single source of persistent truth ([036c-the-raft-storage](036c-the-raft-storage.md)).

When a conflict arises between keeping old code and achieving architectural purity, purity wins.

### What needs Raft-native equivalents not yet built

These Phase 1 features solved real problems. The Raft core from 036 doesn't have the corresponding mechanism yet. They need new implementations inside the Raft algorithm or at the server layer.

- **PreVote** ([034-the-prevote](034-the-prevote.md)) — side-effect-free pre-election prevents a partitioned node from disrupting the cluster with stale term bumps. Our Raft core has direct vote only; 036t explicitly deferred PreVote.
- **CheckQuorum / leader fencing** ([032-the-fence](032-the-fence.md)) — leader detects quorum loss and steps down. Our Raft core has no CheckQuorum. A stale leader will keep serving forever.
- **Leadership transfer** ([035-the-transfer](035-the-transfer.md)) — graceful leadership move via catch-up + `TIMEOUT_NOW`. Our Raft core has no `MsgTimeoutNow` or transfer logic.

### What needs upper-layer reimplementation

These Phase 1 features addressed the server or client layer. The concept survives into the Raft architecture, but the mechanism changes entirely.

- **GET** — read from the Raft-applied state machine. The simplest form is leader-local reads. Linearizable reads (`ReadIndex`) are deferred.
- **Write redirect** ([023-the-write](023-the-write.md)) — follower rejects writes with the leader's address. Raft `Ready` exposes `Lead` (node ID), but node ID ≠ network address. The routing layer needs to map Raft leader identity to a client-reachable address.
- **Staleness bounds** ([022-the-stale](022-the-stale.md), [025-the-bound](025-the-bound.md)) — seq-based lag detection against `primarySeq`. The Raft-native equivalent is applied-index lag: how far behind is this node's state machine vs. the committed index? The concept survives, the mechanism (seq → applied index) changes.
- **Session consistency** ([024-the-session](024-the-session.md)) — `WaitForSeq` let a client read-its-own-writes. The Raft-native equivalent: wait until applied index ≥ the index returned by the client's last write. Same guarantee, different coordinate system.
- **DELETE** — never existed in Phase 1. Needs engine-level `Delete`, `StateMachine` support, and the propose-wait-apply path.
- **HEALTH** — stateless; report Raft state instead of Phase 1 role.
- **`engine.DB` as `StateMachine`** — collapse `sm` and `db` into one. The engine serves the Raft apply loop directly. StateMachine concurrency ([002-the-lock](002-the-lock.md)) needs a defined contract: the apply loop writes serially, GET reads concurrently — the engine's per-bucket sharding already supports this, but the boundary must be explicit.
- **Engine compaction** ([006-the-log-compaction](006-the-log-compaction.md)) — the engine has its own compaction (stop-the-world WAL rewrite). Raft has its own log compaction via snapshots ([036e-the-compaction](036e-the-compaction.md)). Two compaction systems for two different logs. The engine's compaction lifecycle must integrate with Raft's snapshot lifecycle.
- **Bootstrap and restart** ([033-the-bootstrap](033-the-bootstrap.md)) — Phase 1 solved this with `DISCOVERY` and a follower-default rule. Raft nodes rejoin by replaying their persisted log and re-entering the cluster as followers. The startup path needs redesign: static peer list, Raft state recovery, no Phase 1 metafile.
- **Leader routing** ([030-the-discovery](030-the-discovery.md)) — Phase 1's topology broadcast (`TOPOLOGY`) dies in the purge. Raft exposes `Lead` as a node ID. The server needs a mapping from node ID → network address so followers can redirect clients and the CLI can find the leader.
- **kv-server startup flags** — `--replica-of` is a Phase 1 concept. A Raft-based server needs `--node-id`, `--peers`, `--raft-port`. The binary's CLI interface is redesigned for the Raft era.
- **Client layer** — `kv-cli` commands (`promote`, `replicaof`, `--quorum`, `cleanup`) are mostly Phase 1 dead code. Rather than patching, rebuild the CLI for the Raft-era command set — or build a real client library with leader discovery and connection management (the etcd `clientv3` pattern).
- **Integration test scripts** — 17 PowerShell scripts in `scripts/` test Phase 1 scenarios. ~6 are obsolete (epoch, failover, partial-resync, quorum-write, quorum, split-brain). ~9 need rewrites for Raft-native protocol. Only `bench.ps1` survives untouched. The test suite gets the same purge-then-rebuild treatment as the server.

Each reimplementation or Raft-native equivalent is its own episode with its own invariant.

### The north star

When 037 is complete, `Server` is thin: it owns a `RaftHost`, a `StateMachine`, and request routing. Election is Raft. Replication is Raft. Persistence is Raft. The KV store is a function that the log calls, not an actor that calls the log.

## Minimum tests

**Invariant:** every request path that touches state flows through Raft; everything else is stateless.

1. **The server compiles without Phase 1 code** — after the purge, no Phase 1 file (`election.go`, `replication.go`, `fence.go`, `backlog.go`, `peer_manager.go`) exists. No Phase 1 field (`seq`, `lastSeq`, `replicas`, `primary`, `peerManager`, `term`, `votedFor`, `role`, `backlog`, `quorumWrites`, `fenced`) exists on `Server`.
2. **PUT still works through Raft** — three-node cluster, elect via Raft, PUT on leader, value reaches all followers' state machines. The only surviving path from before the purge.
3. **GET reads from the Raft-applied state machine** — PUT a key, GET returns the value. The read path touches only the `StateMachine`, not a separate `db`.
4. **DELETE through Raft** — DELETE a key via propose-wait-apply. GET afterward returns not-found.
5. **HEALTH is stateless** — HEALTH responds with Raft state without touching the log or state machine.
6. **Write to follower is redirected** — PUT on a follower returns the leader's address instead of applying locally or silently dropping.
7. **engine.DB is the StateMachine** — the Raft apply loop writes to the real engine. No separate `sm` and `db` — the engine serves both apply and GET.
8. **Server starts with Raft identity** — kv-server accepts `--node-id`, `--peers`, `--raft-port`. No `--replica-of`.
9. **Restarted node rejoins via Raft** — kill a follower, restart it, it replays its Raft log and catches up from the leader. No Phase 1 discovery protocol.
10. **Engine compaction integrates with Raft snapshots** — engine WAL compaction is triggered as part of the Raft snapshot lifecycle, not independently.
11. **Staleness bounds use applied-index lag** — a follower measures how far behind its applied index is from the committed index, not Phase 1 seq.
12. **Read-your-writes waits on applied index** — client waits until the follower's applied index reaches the index returned by the last write, not Phase 1 seq.
13. **Leader fencing on quorum loss** — a leader that loses contact with a majority of peers steps down. No stale leader continues serving.
14. **PreVote prevents term disruption** — a partitioned node does not increment the cluster term when it reconnects.
15. **Leadership transfer via Raft** — the leader hands off leadership to a caught-up follower through `MsgTimeoutNow`, not an external control path.
16. **Three-node cluster end-to-end** — start three nodes, elect via Raft, PUT + GET + DELETE on leader, all succeed. No Phase 1 subsystem participates.

## Open threads

1. **Linearizable reads** — leader-local reads are not linearizable. A stale leader can serve stale data. `ReadIndex` is the standard solution but is not in scope for 037.
2. **Cluster membership changes** — nodes are fixed at startup. Adding or removing nodes requires Raft conf-change support.
