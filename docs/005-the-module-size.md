# Sabotage
`db.go` is getting large. How do Go developers split a growing system into smaller pieces to improve maintainability and readability?

## Reading up
This is a familiar design problem: split by **responsibility**, not by file size.

Go makes this straightforward because symbol visibility is controlled by naming:
- Uppercase identifiers are exported.
- Lowercase identifiers are package-private.

That means I can split `db.go` into focused files without losing cohesion:
- `wal.go`: the WAL + disk I/O layer
- `shard.go`: the in-memory storage engine (shards, hashing, locking)
- `worker.go`: the strict group commit worker (batching, flush policy, graceful shutdown)