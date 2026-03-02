# kv-go
This repo is my starting point for building things from scratch to understand what’s happening “under the hood”.

By building a key-value store, I want to:
1. Apply what I’ve learned from CS:APP
2. Learn Go by writing real code
3. Learn how modern distributed systems work

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