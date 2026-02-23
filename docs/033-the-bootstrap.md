# Sabotage

Restart any node. It comes up as a sovereign primary, ignores the existing cluster, and starts an election that deposes the healthy leader. The cluster can't survive a single process restart.

## The problem

`--replica-of` is a CLI argument, not persisted state. A restarted replica starts as a standalone primary. `replication.meta` preserves nodeID, term, votedFor, peers — everything except role.

```
t=0  P(term=3), R1, R2 — healthy 3-node cluster
t=1  R1 crashes
t=2  R1 restarts (no --replica-of flag)
t=3  R1 starts as leader (opts.ReplicaOf == "")
t=4  heartbeatLoop: lastHeartbeat is zero → election timeout fires immediately
t=5  R1 sends VOTE_REQUEST(term=4) to peers
t=6  P sees term 4 > 3 → steps down
t=7  Cluster disrupted by a simple restart
```

Three failures compounding:

1. **No role persistence.** `replication.meta` stores nodeID, replid, lastSeq, term, votedFor, peers — but not "I was a follower of X." On restart, `opts.ReplicaOf == ""` → `role = RoleLeader`.

2. **Immediate election.** `lastHeartbeat` initializes to `time.Now()` in `NewServer()`, but the election timeout is 2-4s. By the time `heartbeatLoop` fires, the node is already in leader mode — it skips the follower branch entirely.

3. **No PreVote.** Term increment is unconditional. `becomeCandidate()` bumps term before checking if anyone would vote for it. A stale node with zero data can force a healthy leader to step down just by presenting a higher term.

## Real systems

**etcd — WAL existence is the signal.** `wal.Exist(cfg.WALDir())` is the single boolean that drives every decision (`bootstrap.go:86`). No WAL → first boot (`raft.StartNode` with full peer list). WAL exists → restart (`raft.RestartNode` with zero peers, membership from storage). The restarting node enters follower state, sits quietly, waits for heartbeats. If none arrive within `ElectionTicks` → campaigns normally. No special rejoin protocol. `CheckQuorum: true` is hardcoded (not configurable). `PreVote: true` is the default — prevents a partitioned node from disrupting the cluster by probing "would I win?" before incrementing its term.

**Redis Cluster — `nodes.conf` persists everything.** Each node's identity, role (master/slave), master pointer (`slaveof`), `configEpoch`, `lastVoteEpoch`, and every known peer are written to `nodes.conf` on every state change (`clusterSaveConfig` → temp file → `rename` → `fsync`). On restart, `clusterLoadConfig` restores the full topology. The `"myself"` flag identifies the local node. Reconnection is lazy — `clusterCron()` runs every 100ms, notices `node->link == NULL`, dials peers. No special rejoin command. Role is restored from the persisted file, not discovered.

**Redis Sentinel — external policeman.** After failover, Sentinel adds the old primary's address as a slave entry. When the old primary restarts (as `role:master`), Sentinel detects the mismatch via `INFO` polling, waits `4 × publish_period`, then sends `SLAVEOF <new_master>` in a MULTI transaction to force demotion. The restarted node doesn't self-discover — an external process tells it what to be.

**TiKV — same as etcd, plus PD.** Role is not persisted. On restart, `PeerFsm::create()` reconstructs each region peer from persisted `RaftLocalState` (term, votedFor, log entries) in the Raft engine — structurally identical to etcd's `RestartNode`. Always starts as follower. PD (Placement Driver) handles identity management (store IDs, region routing) but never assigns roles — Raft reconverges organically. `CheckQuorum: true` and `PreVote: true` are defaults (`prevote` via config, `check_quorum` hardcoded unless `unsafe_disable_check_quorum` for disaster recovery). Leader lease (9s, enforced `< election_timeout`) enables local reads without Raft round-trips but is not persisted — created fresh on restart via `Lease::new()`. Cold start: all peers campaign after randomized election timeout, same as etcd.

| | etcd | TiKV | Redis Cluster | Redis Sentinel |
|---|---|---|---|---|
| **Restart signal** | WAL directory exists | `StoreIdent` in KV engine | `nodes.conf` exists | N/A (external) |
| **Role on restart** | Always follower | Always follower (Raft HardState) | Persisted in `nodes.conf` | Former master → Sentinel demotes |
| **Leader discovery** | Raft heartbeats (organic) | PD re-registration + Raft heartbeats | Gossip protocol (100ms cron) | Sentinel sends SLAVEOF |
| **Election safety** | PreVote + CheckQuorum | PreVote + CheckQuorum + leader lease | lastVoteEpoch + rank delay | Sentinel quorum vote |
| **Cold start** | All nodes campaign, randomized timeout | All nodes campaign, randomized timeout | First boot with `--cluster-init` | N/A |

## Design space

### Option A: Persist role in replication.meta

Add `role:follower` and `replicaOf:127.0.0.1:4000` to `replication.meta`. On restart, restore role and reconnect to the persisted primary address.

- ✅ Simplest change (~20 LOC)
- ✅ Matches Redis Cluster approach (persist everything)
- ❌ Persisted primary address may be stale (primary moved, IP changed)
- ❌ Introduces a new failure mode: persisted state conflicts with reality (was leader, peers elected new leader, restarts as leader → split brain)
- ❌ Doesn't solve the fundamental problem: what if the persisted primary is dead?

### Option B: Discovery command (ask peers who's the leader)

On restart with persisted peers, send `CmdDiscovery` to each peer before entering any role. Peers respond with current term + leader info. Node joins as follower of the discovered leader. No peers respond → fall through to election.

- ✅ No stale state — always gets current truth from the cluster
- ✅ Handles primary failover during downtime (discovers new leader)
- ✅ Clean separation: persistence = identity, discovery = role
- ❌ New protocol command (~100 LOC)
- ❌ Network dependency on startup (what if all peers unreachable?)
- ❌ Still needs election fallback for cold start

### Option C: Start as follower, let election converge (etcd approach)

Persisted peers exist → start as follower (not leader). Suppress elections briefly (discovery grace period). If a leader is alive, its heartbeats or `reconcileLoop` will reach this node and establish replication. If no leader contacts this node within the grace period → start election normally.

- ✅ No new protocol command — reuses existing heartbeat + reconcile paths
- ✅ Matches etcd: `RestartNode` always starts as follower
- ✅ Minimal code change: flip the default role when peers exist
- ❌ Relies on the leader's `reconcileLoop` (3s interval) to find it — slow convergence
- ❌ During grace period, the node does nothing (no reads, no writes)
- ❌ Cold start requires all nodes to timeout simultaneously

### Option D: Option C + Discovery command (belt and suspenders)

Start as follower when peers exist. Actively probe peers with `CmdDiscovery` to find the leader faster. If leader found → `relocate()` immediately. If no leader → wait for grace period, then election.

- ✅ Fast convergence (discovery) + safe default (follower)
- ✅ Handles all three scenarios: single restart, leader-dead restart, cold start
- ✅ Discovery is optimization, not requirement — election is the backstop
- ❌ Most complex (~150 LOC)

## The choice: Option D (Follower default + Discovery)

**Rationale:**
- Option A (persist role) is fragile — persisted state lies when the cluster changes during downtime. etcd deliberately ignores `--initial-cluster-state` on restart. Redis Cluster works because gossip corrects stale role within milliseconds. We don't have gossip.
- Option C (pure follower default) is correct but slow. The leader's `reconcileLoop` fires every 3s. The restarting node sits idle for up to 3s + connection time. Discovery cuts this to ~10ms.
- Option B (pure discovery) has no safe fallback — if all peers are unreachable, the node is stuck forever.
- Option D combines the safety of C with the speed of B.

**What we don't do: PreVote.** etcd's PreVote prevents a partitioned node from disrupting the cluster by probing "would I win?" before incrementing its term. We have a simpler substitute: the `fenced` flag from episode 032. A fenced node won't campaign. And the discovery phase ensures a restarting node finds the leader before it even considers an election. PreVote is the right answer for large clusters with complex partition scenarios — for a 3-5 node system, discovery + fenced flag is sufficient. PreVote remains an open thread for later.

## Flow

### Scenario 1: Single node restart, leader alive

```
t=0  R1 restarts, reads replication.meta → has peers → starts as follower
t=1  R1 sends CmdDiscovery to P, R2
t=2  P responds: "term=3, leader=P@127.0.0.1:4000"
t=3  R1 calls relocate("127.0.0.1:4000") → starts replication
t=4  R1 is back in the cluster (~10ms total)
```

### Scenario 2: Single node restart, leader dead

```
t=0  R1 restarts, reads replication.meta → has peers → starts as follower
t=1  R1 sends CmdDiscovery to P (dead), R2
t=2  P: no response. R2 responds: "term=5, leader=R2@127.0.0.1:4002"
t=3  R1 relocates to R2 → starts replication
```

### Scenario 3: Full cluster cold start

```
t=0  P, R1, R2 all restart simultaneously
t=1  All three have peers → start as followers
t=2  All three send CmdDiscovery to each other
t=3  All respond: "term=N, leader=" (no leader — everyone is a follower)
t=4  Discovery grace period expires (~1s)
t=5  heartbeatLoop fires, randomized election timeout breaks the tie
t=6  One node wins election → becomes leader → reconcileLoop recruits others
```

### Scenario 4: First boot (no prior cluster)

```
t=0  Node starts, reads replication.meta → no file (or no peers)
t=1  No peers → skip discovery, start as leader (current behavior unchanged)
```

## Implementation plan

### Phase 1: Default to follower when peers exist

In `NewServer()`, change role assignment:

```
current:  if opts.ReplicaOf == "" { role = RoleLeader }
new:      if opts.ReplicaOf == "" && !hasPeers { role = RoleLeader }
          // hasPeers determined after restoreState()
```

This single change prevents a restarted node from starting as leader. The election system handles the rest — exactly like etcd's `RestartNode` always entering follower state.

### Phase 2: CmdDiscovery protocol

New command `CmdDiscovery = 14`. Request carries the sender's nodeID + term. Response carries:

```
term:<currentTerm>
leader:<nodeID>
leaderAddr:<addr>
```

If the responder doesn't know the leader (is also a follower with no primary), leader and leaderAddr are empty.

Handler: any node can respond. Leaders respond with themselves. Followers respond with their `primaryNodeID` + `opts.ReplicaOf`.

### Phase 3: Discovery on startup

After `restoreState()` and before entering the main loop, if peers exist:

1. Dial each peer via `PeerManager.Get(nodeID)`, send `CmdDiscovery`
2. Collect responses with a short timeout (~500ms)
3. Pick the response with the highest term that names a leader
4. If leader found → `relocate(leaderAddr)`
5. If no leader found → start as follower, let `heartbeatLoop` handle election

### Phase 4: Suppress premature elections

Set `fenced = true` on startup when peers exist. The `heartbeatLoop` already skips elections when `fenced` is set (from episode 032). Discovery or a leader's heartbeat/reconcile clears the flag.

Alternatively, set `lastHeartbeat = time.Now()` after discovery completes, giving the node a full election timeout window before it considers campaigning.

### What changes

| File | Change |
|------|--------|
| `protocol/message.go` | Add `CmdDiscovery = 14` |
| `server/server.go` | `NewServer` / `Start`: default to follower when peers exist; call `discoverCluster()` before main loop |
| `server/handler_cluster.go` | Add `handleDiscovery()` handler; register in `registerRequestHandlers` |
| `server/config.go` | Add `discoveryTimeout` constant (~500ms) |

### Edge cases

- **Discovery during network partition:** All peers unreachable → no leader found → node stays follower → `heartbeatLoop` triggers election after timeout. Same as etcd.
- **Stale discovery response:** Responder reports an old leader that's already dead. `relocate()` will fail to connect → heartbeat timeout → election. Self-correcting.
- **Race: discovery and reconcile both fire.** Both call `relocate()`. Second call is a no-op (already replicating from the same primary) or updates to the newer leader.
- **Split peers disagree on leader:** Pick highest term. If terms are equal, any leader is fine — we'll converge via normal heartbeat/election.
- **Node with `--replica-of` flag:** CLI flag takes precedence over discovery. Existing behavior preserved.

## Design lessons

**The bootstrap problem is universal.** Every distributed system must answer: "what happens when a node restarts?" The answer always involves some combination of persisted state + discovery protocol + election fallback. etcd uses WAL existence. Redis uses `nodes.conf`. Kafka uses bootstrap servers. Cassandra uses seed nodes. The naming varies; the structure is identical.

**Don't persist role, persist identity.** Role is ephemeral — it changes on failover, promotion, demotion. Identity (nodeID, term, votedFor, peers) is durable. etcd doesn't persist "I was a leader" — it persists the Raft log and lets the protocol reconverge. Redis Cluster does persist role, but gossip corrects mistakes within 100ms. Without gossip, persisting role creates a window where persisted state disagrees with cluster reality.

**Start safe, discover fast.** The default on restart should always be the least-disruptive option. For a leader-follower system, that's follower. etcd: `RestartNode` always enters follower state. A discovery command is an optimization to speed up convergence, not a requirement for correctness. The election algorithm is the backstop.

**PreVote is the production answer.** Our discovery + fenced flag is sufficient for small clusters, but PreVote (Raft thesis §9.6) is the general solution: don't increment your term until you've confirmed you could win. etcd enables it by default. We'll revisit it when the cluster grows beyond 3-5 nodes or when we tackle multi-region deployments.
