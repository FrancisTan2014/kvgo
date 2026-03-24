# 036s — The Raft Transport

The Raft algorithm works. A leader proposes, followers step, entries commit, state applies. Every episode since 036b proved this — through unit tests that pass messages by calling `Step` directly in the same process.

**What if two nodes are in separate processes?**

`handleBatch` calls `transport.Send(rd.Messages)`, but the only `RaftTransporter` is a mock that captures messages into a channel. They never reach the wire. The cluster is a single-process theater — consensus with no one to actually agree with.

The sabotage is simple: start two nodes, propose a write on node A, and node B never sees it. Not because Raft is broken, but because there is no transport.

## Boundary

036s builds the Raft transport — the layer that moves `raftpb.Message` between processes over TCP.

In scope:
- `transport/raft` package implementing `RaftTransporter`
- `proto.Marshal` / `proto.Unmarshal` over length-prefixed framing (`Framer`)
- One outbound TCP connection per peer, dialed lazily on first `Send`
- A TCP listener for inbound connections
- A `Raft` interface (`Process`) for delivering inbound messages to the Raft layer
- Per-peer write goroutine (channel-decoupled from `Send`) and read goroutine

Deferred:
- Reconnection logic — if a connection drops, the peer is unreachable until the next `Send` redials. Raft retries via heartbeat ticks
- Connection deduplication — two nodes dialing each other creates two connections per pair. Tolerated; both deliver to `Process` correctly
- Backpressure — if the per-peer write channel is full, the message is dropped. Raft retries
- Snapshot transfer — `MsgSnap` metadata fits in a message, but the full state machine image does not

Out of scope:
- TLS, authentication
- Client-facing protocol changes
- Cluster membership changes

## Design

### Separation from client traffic

Client traffic is request-response: every request has a matched response. Raft traffic is fire-and-forget: messages are independent in both directions. The existing `MultiplexedTransport` pairs requests by ID — Raft messaging has no such pairing.

`transport/raft` owns its own listener and connections. It does not touch the client protocol. etcd makes the same split: `:2379` for clients, `:2380` for peers.

### The `Raft` interface

The transport needs to deliver inbound messages somewhere. A `Raft` interface represents the consumer:

```go
type Raft interface {
    Process(ctx context.Context, m *raftpb.Message) error
}
```

This starts with one method. When the transport learns things the Raft layer needs to know — peer went down, snapshot transfer failed — the interface grows (`ReportUnreachable`, `ReportSnapshot`). The server's `raftHost.Step()` satisfies `Process`. The transport doesn't know that — it holds the interface.

### Codec

`proto.Marshal(msg)` produces bytes. `Framer.Write(bytes)` puts them on the wire with a 4-byte LE length prefix. On the read side, `Framer.Read()` returns the next complete frame, and `proto.Unmarshal(bytes, msg)` reconstructs the message.

TCP is a byte stream, not a message stream. It guarantees ordering and delivery (or error), but has no concept of message boundaries. That's why the Framer exists — the length prefix creates message boundaries on top of the stream. When a connection drops mid-frame, the reader's `io.ReadFull` returns `io.ErrUnexpectedEOF`. The partial frame is discarded. Raft retransmits on the next heartbeat.

### Peer model

The server owns the topology. It registers peers via `AddPeer(id, addr)` after construction:

```go
transport.AddPeer(2, "10.0.0.2:2380")
```

`Send(msgs)` groups messages by `msg.To`, looks up the peer, finds or dials the connection, marshals, and writes. Unknown destinations are logged and dropped — Raft retries.

Connections are dialed lazily on the first `Send` to that peer. No eager connect-at-start.

### Connection lifecycle

One outbound TCP connection per peer. Each connection has:

- A **write goroutine** that takes messages from a channel, marshals, and writes to the Framer
- A **read goroutine** that reads frames, unmarshals, and calls `Raft.Process`

The channel decouples `Send` (called from the Raft event loop) from the network write (which may block on TCP backpressure). `Send` never blocks on the network.

Inbound connections from the listener also spawn a read goroutine. The first message identifies the sender via its `From` field — no separate handshake. Simple but not secure; authentication is deferred.

`Start()` binds the listener. `Stop()` closes the listener and all peer connections.

## Minimum tests

**Invariant:** a `raftpb.Message` sent by one node arrives at the destination node with all fields intact.

1. **Send and receive.** Two transports on loopback. Node A sends a `MsgApp` with entries, node B's `Process` callback receives it with identical fields. Proves the invariant end-to-end over TCP — the sabotage from the opening is resolved when this passes.

2. **Bidirectional.** A sends to B, B sends to A, both receive. Proves the invariant holds in both directions on the same connection pair.

3. **Unknown peer dropped.** Send a message to a node ID not in the peer map. Assert no panic, no hang. Proves the transport degrades gracefully without violating the invariant for known peers.

4. **Stop is clean.** Start a transport, send some messages, stop it. Assert the listener closes and goroutines exit. Proves the transport can be torn down without leaking resources that would break the invariant on restart.

## Open threads

1. **Dual connections.** Two nodes dialing each other creates two connections per pair. Both deliver messages correctly. Deduplication (etcd-style stream negotiation) is deferred.

2. **Backpressure.** A slow peer causes dropped messages. Monitoring or adaptive channel sizing is deferred.

3. **Snapshot transfer.** Full state machine images don't fit in a single message. A separate transfer mechanism is needed when snapshot restore is implemented.

4. **TLS and authentication.** The listener accepts any connection. A malicious node can claim any `From` ID.
