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

Episodes 001-019 focus on server survival (crashes, replication, backlog).
Episodes 020-025 focus on client experience (where to write, stale reads, session guarantees).

Example: Episode 012 (the-crash) → AOF for crash recovery
Example: Episode 025 (the-staleness) → Bounded staleness for replica reads

This approach teaches distributed systems through concrete problems, not abstract theory.