# 037d — The Reconnect

Kill a follower, restart it. The surviving peers never re-dial — `Peer.connected` stays `true` on a dead socket, `writeLoop` exited silently, and outbound messages vanish. The restarted node starves.

Phase 1 solved this in 018 — `replicationLoop` retried connections indefinitely with backoff, context cancellation handled the relocate race, and full resync recovered state. The Raft transport (036s) shipped without any of that. This episode brings Raft transport to parity.

## Boundary

Rebuild per-peer connection lifecycle: detect dead connections, re-establish them automatically, never block the Raft event loop.

In scope:
- Writer-dials-acceptor-reads ownership model
- `Peer` interface — transport-mechanism-agnostic abstraction
- `tcpPeer` — raw TCP + Framer, background reconnect with exponential backoff
- Non-blocking `Send` — drop when no connection, never block the node loop
- Accept-triggers-reset — inbound connection from a known peer wakes the outbound writer

Deferred:
- HTTP transport (`httpPeer`) — future migration for auth, headers, TLS
- Connection deduplication — two per pair is fine at 3–5 nodes
- `ReportUnreachable` — notifying Raft that a peer is down
- Backpressure / adaptive channel sizing

Out of scope:
- TLS, authentication
- Cluster membership changes (ConfChange)

## Design

### Why the current design breaks

Three bugs compound:

1. **`writeLoop` exits silently on write error.** No cleanup, no state change.
2. **`connected` is never reset.** `sendTo` sees `true`, pushes into a dead channel.
3. **Dial blocks the node loop.** `dialPeer()` runs synchronously inside `sendTo` — a TCP timeout stalls the entire Raft pipeline.

### The ownership model

etcd splits each peer into a `streamReader` (dials, reads) and `streamWriter` (waits for connection, writes). Over HTTP, the URL identifies the peer — the reader dials and the server routes the response to the writer. Raw TCP has no URL, so we invert the split: **the writer dials, the acceptor reads.**

- **Writer** — dials the remote peer, writes from `msgc`. On error: close, backoff, re-dial. Owns its dialed connection.
- **AcceptLoop** — accepts inbound connections, reads the first message's `From` field to identify the peer, spawns a `readLoop` for continued reading.

Each pair has two TCP connections (one per direction), each unidirectional. The dialer writes, the acceptor reads. At 5 nodes, 10 extra sockets — irrelevant.

### The `Peer` interface

```go
type Peer interface {
    Send(msgs []*raftpb.Message)
    Stop()
}
```

Transport holds `map[uint64]Peer`. `tcpPeer` is the concrete implementation. Future `httpPeer` implements the same interface — the seam for HTTP migration.

### `tcpPeer`

```
tcpPeer {
    id, addr
    msgc     chan *raftpb.Message   // Send → writerLoop
    resetc   chan struct{}          // acceptLoop → writerLoop (wake from backoff)
    stopc    chan struct{}
}
```

**`writerLoop`:** dial → write from `msgc` → on error: close, backoff, re-dial. Backoff `select` includes `<-resetc` for immediate wake.

**`Send`:** non-blocking push to `msgc`. Full? Drop. Raft retransmits.

### Accept-triggers-reset

`acceptLoop` reads the first message's `From` field → looks up peer → calls `Reset()`. The writerLoop wakes from backoff and re-dials immediately.

### Conformance suite

Tests 1–3 and 7 prove the `Peer` interface invariant, not `tcpPeer` internals. They live in a shared suite parameterized by a factory:

```go
type SingleTransportFactory func(t *testing.T, cfg RaftTransportConfig, raft *mockRaft) *RaftTransport
```

Each conformance function creates its own transports via the factory, controlling listener binding and lifecycle per-test. Any future `Peer` implementation (e.g. `httpPeer`) must pass the same suite. The compiler enforces the interface; the conformance suite enforces the behavior. If `httpPeer` ships without running the suite, 037d’s invariant is broken.

Tests 4–5 are `tcpPeer`-specific (backoff timing, `resetc` wakeup). They test internal mechanics, not the contract.

### Backoff

Exponential from 50ms to 1s with jitter. Reset on successful dial.

## Minimum tests

**Invariant:** a message sent to a temporarily unreachable peer is eventually delivered after recovery, without blocking the sender's event loop.

1. **Send succeeds after peer starts late** *(conformance)* — A sends to B (not started). B starts. Message arrives. Proves reconnect works.
2. **Send is non-blocking when peer is down** *(conformance)* — send to a down peer, assert `Send` returns immediately.
3. **Writer recovers after connection drop** *(conformance)* — A↔B connected. Kill B. Restart B. Messages flow again. Proves full lifecycle.
4. **Accept triggers fast reconnect** *(tcpPeer)* — B's writerLoop dials A. A's acceptLoop identifies B, wakes A's writerLoop(B) from backoff.
5. **Backoff increases on repeated failure** *(tcpPeer)* — dial a permanently down peer, assert increasing delays.
6. **Existing 036s tests still pass** — send-receive, bidirectional, unknown peer drop, clean stop.
7. **Stop is clean** *(conformance)* — stop transport with active reconnect loops. All goroutines exit, no leaked connections.

## Open threads

1. **`ReportUnreachable`** — notify Raft when a peer is down so the leader pauses replication attempts.
2. **HTTP migration** — `Peer` interface is the seam. `httpPeer` gains auth/TLS without changing the transport. `AddPeer` currently hardcodes `newTCPPeer` — a `PeerFactory` field on config would be the wiring seam.
3. **Inbound connection cleanup** — `acceptLoop` doesn’t track read goroutines per peer. A reconnecting node can leave orphaned goroutines until TCP keepalive detects the dead connection (75–120s).
4. **Listener ownership** — `Stop()` unconditionally closes the listener, even when injected via `cfg.Listener`. If the transport sits behind a connection mux, this kills the shared listener. Fix: never close an injected listener (caller owns it).
5. **`RemovePeer`** — no way to remove a peer. The writerLoop dials forever with backoff, the `peers` map entry persists, `msgc` drains to nothing. Required for ConfChange.
