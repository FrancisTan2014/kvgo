# Sabotage
RequestTransport serializes all requests with mutex. One connection per purpose, or accept terrible throughput.

## The failure
```
Current TcpRequestTransport (Episode 028):

func (t *TcpRequestTransport) Request(payload []byte, timeout time.Duration) ([]byte, error) {
    t.mu.Lock()              // ← ALL requests serialize here
    defer t.mu.Unlock()
    
    t.framer.Write(payload)
    resp := t.framer.Read()  // ← Can't tell which response belongs to which request
    return resp, nil
}

Problem:
- Quorum read sends 3 concurrent requests to 3 replicas: OK (different transports)
- But 2 concurrent requests to SAME replica: serialized (mutex blocks)
- Topology broadcast + quorum read to same peer: serialized
```

**Current workaround:**
```
Create separate connection for each purpose:
- Connection 1: Replication (StreamTransport)
- Connection 2: Control plane (RequestTransport for TOPOLOGY)  
- Connection 3: Data queries (RequestTransport for quorum reads)

3-node cluster = 9+ connections
```

**Root cause:** No way to correlate responses to requests. Line 149 reads next bytes from socket, but which request do they belong to?

## The constraint

**TCP is full-duplex, but ordering matters:**
```
Node A → B connection:
  A sends: [GET key1][GET key2][TOPOLOGY]
  B sends: [ValueA][ValueB][PeerList]
  
Question: When A reads "ValueA", is this:
  - Response to key1?
  - Response to key2?
  - Response to TOPOLOGY?
  
Answer: FIFO ordering assumes responses match request order
But if B reorders (key2 faster than key1), A is confused.
```

**Command type alone fails:**
```
A sends: [GET key1][GET key2]
B sends: [GET value="foo"][GET value="bar"]

Both are GET responses. Which is key1, which is key2?
```

## Real systems

**HTTP/1.1:** Sequential (like current mutex)
- Send request, wait for response, send next
- Simple, but slow

**HTTP/2:** Multiplexing with stream IDs
- Each request gets unique stream ID
- Responses include same ID
- Concurrent requests pipelined

**gRPC:** Built on HTTP/2
- Request/response correlation via stream IDs
- Bidirectional streams
- Flow control per stream

**Redis:** Pipelining without IDs
- Responses MUST match request FIFO order
- Server processes in order, replies in order
- Client correlates by position

**QUIC:** Multi-stream transport layer
- Each stream independent
- Head-of-line blocking eliminated

## Design space

### Option A: Keep separate connections (current plan)
```
replicaConn: StreamTransport for replication
reachableNodes: map[string]RequestTransport for control/queries

2-3 connections per node pair
```
- ✅ Simple, no protocol changes
- ❌ Connection explosion (N nodes × M purposes)
- ❌ TCP overhead per connection

### Option B: Add request IDs (HTTP/2 style)
```
Protocol:
  [RequestID:4][Command:1][Flags:1][Payload...]
  [RequestID:4][Status:1][Payload...]

MultiplexedTransport:
  - nextID atomic counter
  - pending map[RequestID]chan Response
  - Background readLoop() routes responses to channels
```
- ✅ 1 connection per node pair
- ✅ Concurrent requests pipelined
- ✅ Clean semantics (request ID explicit)
- ❌ Protocol breaking change
- ❌ ~200-300 LOC complexity

### Option C: Redis-style FIFO pipelining
```
Queue requests, correlate by order:
  sentQueue: []RequestMetadata
  When response arrives: pop sentQueue, match to oldest
```
- ✅ No protocol change (no request ID needed)
- ✅ Some concurrency (pipeline depth)
- ❌ Requires server FIFO guarantee
- ❌ Head-of-line blocking (slow request blocks all)

## The choice: Option B (Request IDs)

**Why:** 
- Episode 028 transport abstraction enables this
- Clean architectural boundary (protocol vs transport)
- Prepares for HTTP/2 transport (same model)
- Better than connection explosion

**Tradeoff:** Protocol breaking change, but we're pre-1.0. Worth doing right now.

## Implementation

**Protocol changes:**
- Add RequestID field (4 bytes) to Request and Response
- RequestID=0 reserved for no-response-expected (replication stream)
- Wire format: [RequestID:4][Cmd:1][Flags:1][Seq:8][Payload...]

**MultiplexedTransport structure:**
- nextID: atomic counter for ID generation
- pending: map[RequestID]chan Response for correlation
- readLoop: background goroutine routes responses to waiting channels
- writeMu: protects concurrent writes

**Key methods:**
- Request(): allocate ID → register channel → send → wait on channel or timeout
- readLoop(): read from socket → extract ID → route to pending[ID] channel
- allocateID(): check pending map to avoid collision after wraparound
- Close(): drain pending requests with 5s timeout (graceful shutdown)

**Flow control:**
- inflightSem: buffered channel as semaphore (max in-flight limit)
- Blocks new requests when limit reached (backpressure)
- HTTP/2 uses WINDOW_UPDATE frames; we use simpler semaphore

**Replication stream:**
- RequestID=0 for PUT/DELETE operations (no response expected)
- ACK/NACK include RequestID matching the operation they acknowledge

**Tests:**
- Concurrent requests (no serialization)
- Timeout cleanup
- Connection close (all pending fail)
- RequestID wraparound
- Flow control backpressure
- Graceful shutdown

## What we learn

- Request/response correlation via IDs. 
- Message multiplexing over single connection. 
- Background goroutine for async I/O. 
- Channel-based sync semantics. 
- TCP full-duplex utilization. 
- Flow control and backpressure. 
- Graceful degradation. ID space management.
