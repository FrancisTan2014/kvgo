# Sabotage
What if our server's hard drive melts?

The data is gone. `fsync` guards against process crashes, not hardware failure.

## Analysis
We need a replica: a second server that holds a copy of all writes.

When a replica starts, it connects to the primary with a `replicate` command. The primary tracks this connection and, after each `Put` commits, forwards the write to all replicas. The replica rejects `Put` requests but accepts `Get` requests (with eventual consistency), and applies forwarded writes to its own WAL.

Since primary and replica share most logic (WAL, storage, network), we reuse the same binary with a flag (`-replica-of <addr>`) to select the role. This keeps deployment simple and avoids code duplication.

With two nodes, we've officially entered the world of distributed systems. 😊

## What to do

1. Add `ReplicaOf` option to `server.Options` — if set, the server runs as a replica and connects to the given primary address
2. Add `-replica-of <addr>` flag to `kv-server` to enable replica mode
3. Extend the protocol with a `Replicate` opcode for the replica→primary handshake
4. Primary tracks replica connections and forwards committed writes
5. Create a benchmark script to orchestrate primary + replica + bench

## Architecture

```
┌─────────────┐         ┌─────────────┐
│   Client    │         │  kv-bench   │
└──────┬──────┘         └──────┬──────┘
       │ GET/PUT               │ PUT
       ▼                       ▼
┌─────────────────────────────────────┐
│            Primary                  │
│  ┌─────┐  ┌─────┐  ┌──────────────┐ │
│  │ WAL │◄─│ DB  │◄─│ Client Conns │ │
│  └──┬──┘  └─────┘  └──────────────┘ │
│     │                               │
│     │ forward writes                │
│     ▼                               │
│  ┌──────────────┐                   │
│  │ Replica Conns│                   │
│  └──────┬───────┘                   │
└─────────┼───────────────────────────┘
          │
          ▼
┌─────────────────────────────────────┐
│            Replica                  │
│  ┌─────┐  ┌─────┐                   │
│  │ WAL │◄─│ DB  │ (read-only)       │
│  └─────┘  └─────┘                   │
└─────────────────────────────────────┘
```

## Open questions

- **Sync vs async**: Does the primary wait for replica ack before responding to clients?
- **Catch-up**: If a replica reconnects after downtime, how does it get missed writes?
- **Read scaling**: Can clients query the replica, accepting eventual consistency?