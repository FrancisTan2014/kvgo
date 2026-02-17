# Sabotage
Replicas don't know about each other. Quorum reads from replicas silently degrade to local-only reads.

## The problem

**Information asymmetry:** Primary passively receives replica connections and builds a complete peer list. Replicas only connect to the primary - they're blind to their peers.

**Impact:** 3-node cluster where Replica1 attempts quorum read:
- Primary knows: `[replica1, replica2]`
- Replica1 knows: `[]` (empty)
- Quorum = majority of 1 node = 1 node
- Result: "quorum read" is actually eventual read (no safety guarantee)

**Why replication doesn't help:** Replication uses StreamTransport (unidirectional data flow). Quorum reads need RequestTransport to each peer for request-response pattern. No discovery protocol exists to share peer addresses or trigger connection establishment.

## Real systems

**Redis Sentinel:** External service discovery
- Sentinels monitor primaries + replicas
- Replicas learn topology from Sentinel queries
- Used for monitoring, not data queries

**Redis Cluster:** Gossip protocol
- Each node maintains full cluster topology
- Periodic PING/PONG with random peers (Gossip)
- Eventually consistent view of cluster
- ~16k node slots mapped to nodes

**etcd:** Raft-based consensus
- All nodes are peers (no primary-replica split)
- Topology embedded in Raft membership
- Join via explicit member-add operation
- Strongly consistent membership

**Cassandra:** Gossip protocol
- Every node knows about every other node
- Gossip protocol (anti-entropy)
- Seed nodes for bootstrap
- Eventually consistent membership

**MongoDB:** Replica set configuration
- Primary broadcasts replica set config
- Secondaries discover each other via config
- Reconfiguration via replSetReconfig command

## Design space

### Option A: Static configuration file
```yaml
cluster:
  nodes:
    - address: "primary:6379"
    - address: "replica1:6379"
    - address: "replica2:6379"
```
- ✅ Simple: read file at startup
- ✅ No protocol changes
- ❌ Requires manual config management (editing files, restarts)
- ❌ No dynamic discovery (adding replica = editing all configs)
- ❌ Not cloud-native (IPs change, containers ephemeral)

### Option B: Gossip protocol (Cassandra/Redis Cluster style)
```
Each node periodically sends GOSSIP to random peers:
  [nodeId][address][state][knownPeers]

Eventual consistency: 
  - Node joins → gossips to seeds → spreads through network
  - ~O(log N) convergence time
```
- ✅ Highly resilient (no single point of failure)
- ✅ Handles network partitions gracefully
- ✅ No central coordination needed
- ❌ Complex: 500-800 LOC (memberlist, state reconciliation, conflict resolution)
- ❌ Eventually consistent (temporarily incomplete view)
- ❌ Doesn't match our single-primary architecture (primary-replica asymmetry)

### Option C: Primary broadcasts topology (MongoDB style)
```
Primary maintains authoritative peer list.
When replica connects or topology changes:
  Primary → All replicas: TOPOLOGY [replica1:6379, replica2:6379, ...]

Replicas:
  - Receive TOPOLOGY message
  - Establish RequestTransport to each peer
  - Update reachableNodes map
```
- ✅ Centralized truth (matches existing single-primary model)
- ✅ Simple: ~150 LOC (broadcast + handler)
- ✅ Immediate consistency (primary is source of truth)
- ✅ Clean fit: primary already knows all replicas (handleReplicate)
- ❌ Single point of failure (if primary dies, topology frozen)
- ❌ Doesn't handle split-brain (but we don't solve that yet anyway)

### Option D: Heartbeat piggyback
```
Already sending heartbeats primary → replica.
Piggyback peer list in heartbeat payload:
  [primarySeq][peer1:6379][peer2:6379]...
```
- ✅ Reuses existing mechanism (no new command)
- ✅ Eventually consistent (periodic updates)
- ❌ Heartbeats are one-way (primary → replica), replicas can't share with each other
- ❌ Still need discovery protocol when replica joins
- ❌ Mixes concerns (heartbeat = liveness, topology = membership)

## The choice: Option C (Primary Broadcast)

**Rationale:**
- Matches existing single-primary topology (not peer-to-peer mesh)
- Gossip protocol is overkill for star topology (1 primary + N replicas)
- Primary already knows all replicas (they connect via REPLICATE)
- Simpler than gossip: ~150 LOC vs 500-800 LOC
- Immediate consistency (primary is authoritative source)

**Tradeoffs:**
- Primary is single point of failure for discovery (already SPOF for writes)
- Split-brain not handled (deferred to consensus/fencing layer)
- Not extensible to peer-to-peer mesh (acceptable for current model)
- Topology frozen if primary dies before promotion

## Implementation

**Protocol:** Add TOPOLOGY command (newline-delimited peer addresses in payload)

**Primary broadcasts when:**
- Replica connects/disconnects
- REPLICAOF command changes role

**Replica handling:**
- Parse peer list from TOPOLOGY message
- Skip self and existing connections
- Dial RequestTransport to each peer
- Update reachableNodes map

**Thread safety:** Existing helpers (addReachableNode, removeReachableNode, getReachableNodesSnapshot) already mutex-protected

**Edge cases:**
- Idempotent: duplicate broadcasts safe (check before adding)
- Failed dial: log warning, continue (peer may be down)
- Stale topology if primary dies before promotion (acceptable)

## Design lessons

**Centralized vs decentralized discovery:**
- Gossip (Cassandra/Redis Cluster): Resilient, eventually consistent, complex
- Centralized broadcast (MongoDB): Simple, immediate consistency, single point of failure
- Choice depends on topology: peer-to-peer mesh favors gossip, single-leader favors broadcast

**Broadcast patterns:**
- Push model: Primary actively notifies replicas on topology change
- Idempotent updates: Duplicate messages safe (check reachableNodes before adding)
- Graceful degradation: Failed dial to peer doesn't block others

**On-demand connection establishment:**
- RequestTransport established lazily when TOPOLOGY received
- Avoids connection explosion (don't dial until needed)
- Cleanup on disconnect prevents stale connections

**Expected outcomes:**
- Quorum reads from replicas gain linearizability (R+W>N property enforced)
- Discovery latency: ~10-15ms at startup (one-time cost per topology change)
- No change to primary behavior (already had full topology)
