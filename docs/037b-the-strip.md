# 037b — The Strip

The dual system — Phase 1 running alongside Raft — cannot be tested, cannot be reasoned about, and cannot be made correct. 037a established the plan: purge first, reimplement second.

This episode executes the purge.

## Boundary

Delete all Phase 1 code. After this episode, only PUT through Raft survives. GET, DELETE, and every other command are reimplemented in subsequent episodes.

In scope:
- Delete Phase 1 files: election, replication, fencing, backlog, peer management, heartbeat, cluster handlers, quorum coordination
- Delete Phase 1 test files
- Delete obsolete integration test scripts
- Strip Phase 1 fields and methods from `Server`
- Strip Phase 1 handler registrations from dispatcher
- Strip Phase 1 protocol constants
- Strip Phase 1 config constants
- Simplify `handlePut` to Raft-only path
- Stub or remove `handleGet` (reimplemented in 037c)

Deferred:
- GET reimplementation (037c)
- All other reimplementation work (per 037a plan)

Out of scope:
- Adding any new functionality
- Modifying Raft core code

## Design

No design. This is mechanical deletion guided by 037a's inventory.

## Minimum tests

**Invariant:** the server compiles without Phase 1 code, and PUT through Raft still works.

1. **The server compiles** — `go build ./...` succeeds with no Phase 1 files present.
2. **Raft tests pass** — `go test ./raft/...` is unaffected by the purge.
3. **PUT through Raft** — the 036u wiring test (three-node cluster, PUT on leader) still passes.

## Open threads

None. The purge creates no new questions — it only removes old answers.
