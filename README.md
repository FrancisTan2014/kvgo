# kv-go

This repo is my starting point for building things from scratch to understand what’s happening “under the hood”.

By building a key-value store, I want to:
1. Apply what I’ve learned from CS:APP
2. Learn Go by writing real code
3. Learn how modern distributed systems work

## Articles

Small essays distilled from the build process:

- [Stop Before the Boundary and Prove What You Have](docs/articles/stop-before-the-boundary.md) — the first lesson that became concrete for me in the Raft rewrite: find the seam, shrink the subject, prove it, then move forward.
- [Software Design Moves Top-Down, Bottom-Up, and Back Again](docs/articles/back-and-forth-in-software-design.md) — how the 036 series taught me that real software design does not move in one direction; it stabilizes through repeated movement between architectural direction and implementation pressure.

## Current status

As of episode `037h`, `kv-go` is a Raft-owned distributed key-value store in active construction.

What works today:
- three-node Raft cluster with tick-driven election and TCP transport
- `PUT` flows through propose → majority agreement → apply
- restart recovery from durable storage
- segmented WAL with automatic compaction
- dedicated heartbeat messages (`MsgHeartbeat` / `MsgHeartbeatResp`) preparing the path for linearizable reads

Still open:
- `ReadIndex` / linearizable `GET`
- membership changes and learners
- snapshot transfer and more client-facing polish

## Sabotage
I think of this as a question-driven methodology: I start with questions and let them guide me through distributed
systems. This helps me avoid getting lost in details and build a solid mental model faster.

**The pattern:** Every feature exists because a specific failure mode needs detection and recovery.

Example: Episode 012 (the-crash) → AOF for crash recovery
Example: Episode 025 (the-staleness) → Bounded staleness for replica reads

This approach teaches distributed systems through concrete problems, not abstract theory.

## Episodes

**Phase 1 — The Redis-like store (episodes 001–035)**

Episodes 001–019 focus on server survival (crashes, replication, backlog).
Episodes 020–025 focus on client experience (where to write, stale reads, session guarantees).
Episodes 026–035 bolt consensus onto the store: quorum writes, quorum reads, leader fencing, failover, split-brain, leader transfer. Each one is a separate mechanism, wired into the server ad-hoc.

**Episode 036 — The watershed**

Episode 036 (the-raft) is not another feature. It's an identity change. The system stops being a key-value store that bolts on consensus and becomes a consensus system with a key-value state machine. The database drops from the center of the architecture to the edge. The log takes the center. Everything before 036 is a Redis-like system. Everything after is an etcd-like system.

**Phase 2 — The consensus system (episodes 036–)**

From here, every write enters through the replicated log, gets majority agreement, then reaches the database. Adding a new consensus feature means defining a new entry type — not building a new fan-out, counter, and quorum check.

## References & Acknowledgements

`kv-go` is a learning-first project built with heavy inspiration from excellent open source systems.

- Raft paper (Ongaro & Ousterhout) — core consensus model reference: https://raft.github.io/raft.pdf
- Raft dissertation (Diego Ongaro) — deeper design rationale and proof details: https://github.com/ongardie/dissertation

- `etcd` — Raft-driven distributed KV architecture and production-grade failure handling: https://github.com/etcd-io/etcd
- `raft` (etcd-io) — standalone Raft library design and algorithm implementation reference: https://github.com/etcd-io/raft
- `redis` — practical single-primary replication model, simplicity, and operational pragmatism: https://github.com/redis/redis
- `tikv` — large-scale distributed KV design and storage/consensus integration ideas: https://github.com/tikv/tikv
- `bbolt` — embedded storage engine concepts relevant to local persistence and indexing: https://github.com/etcd-io/bbolt

Thanks to the maintainers and contributors of these projects for publishing deeply educational systems and code.