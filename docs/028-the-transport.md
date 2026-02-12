# Sabotage
Server coupled to TCP. Want to add quorum reads over existing connection. Connection already owned by replication loop reading frames.

## The failure
```
Episode 027 implementation attempt:

doQuorumGet() tries to:
  1. Get reachableNodes map[net.Conn]*protocol.Framer
  2. For each connection, write request frame
  3. Read response frame
  
Problem:
  - Connection owned by serveReplicaWriter (primary→replica)
  - serveReplicaWriter doing writes (forwarding operations)
  - doQuorumGet tries concurrent write+read
  - Result: "use of closed network connection"
```

**Root cause:** Mixed communication patterns on same connection:
- Replication: one-way streaming (primary→replica, no responses)
- Quorum reads: request-response (need bidirectional)

After studying the problem: Even with thread-safe Framer, Server is **tightly coupled to TCP primitives**.

## The constraint

Server code is full of:
```go
conn net.Conn
framer := protocol.NewConnFramer(conn)
conn.SetWriteDeadline(...)
framer.Write(payload)
```

**Problems:**
1. **Protocol lock-in:** Can't add HTTP/2, QUIC, Unix sockets without rewriting Server
2. **Thread safety unclear:** Who locks Framer? Caller or Framer itself?
3. **Pattern confusion:** Streaming vs request-response both use same Framer
4. **Testing:** Need real TCP connections, can't easily mock

**What breaks if we want HTTP/2:**
- Server imports `net` directly
- Uses TCP-specific `SetDeadline`
- Assumes length-prefixed framing (HTTP/2 uses frames differently)
- Would need to rewrite every connection handling in Server

**The question:** How do you decouple business logic from protocol details?

## Design options

### Option A: Protocol-specific abstractions
```go
type TcpTransport interface { ... }
type HttpTransport interface { ... }
type QuicTransport interface { ... }
```

**Tradeoff:**
- ❌ Server must handle each protocol differently
- ❌ Adding protocol = changing Server code
- ✅ Can expose protocol-specific features

### Option B: Unified transport interface
```go
type Transport interface {
    Send([]byte) error
    Receive() ([]byte, error)
    Close() error
}
```

**Tradeoff:**
- ✅ Server protocol-agnostic
- ✅ Add protocols without touching Server
- ✅ Easy testing (mock transport)
- ❌ Lowest common denominator features

### Option C: Semantic separation
```go
// Streaming: replication, client connections
type StreamTransport interface {
    Send([]byte) error
    Receive() ([]byte, error
}

// Request-response: quorum reads, RPC
type RequestTransport interface {
    Request([]byte, timeout) ([]byte, error)
}
```

**Tradeoff:**
- ✅ Clear semantics (intent visible in type)
- ✅ Protocol-agnostic
- ✅ Different transports for different patterns
- ✅ Request-response can optimize (connection pooling, multiplexing)
- ❌ More interfaces to implement

## The choice: Option C (Semantic separation)

**Why:**
1. **Episode 027 taught us:** Streaming and request-response are fundamentally different
2. **Type safety:** `StreamTransport` signals "one-way or bidirectional stream"
3. **Future-proof:** Can add HTTP/2 multiplexing to RequestTransport without breaking StreamTransport
4. **Clear ownership:** StreamTransport = one owner, RequestTransport = concurrent requests OK

**Implementation:**

```go
// src/transport/transport.go
type StreamTransport interface {
    Send(payload []byte) error
    Receive() ([]byte, error)
    Close() error
    RemoteAddr() string
}

type RequestTransport interface {
    Request(payload []byte, timeout time.Duration) ([]byte, error)
    Close() error
    RemoteAddr() string
}
```

**TCP implementation:**
```go
// src/transport/tcp.go

// Streaming: replication, client connections
type TcpStreamTransport struct {
    conn   net.Conn
    framer *protocol.Framer
    readMu sync.Mutex   // Protects bufio.Reader
    writeMu sync.Mutex  // Protects bufio.Writer
}

// Request-response: quorum reads
type TcpRequestTransport struct {
    conn   net.Conn
    framer *protocol.Framer
    mu     sync.Mutex   // Serializes request-response cycle
}
```

## Thread safety details

**Problem from Episode 027:** Is Framer thread-safe?

**Answer:** Framer wraps `bufio.Reader` and `bufio.Writer`, which are **NOT thread-safe**.

**Why mutexes needed:**
```go
// Without mutex:
Goroutine 1: framer.Write(msg1) 
  → bufio.Writer.Write() → internal buffer[0:100]
  
Goroutine 2: framer.Write(msg2) [CONCURRENT]
  → bufio.Writer.Write() → corrupts buffer[50:150]
  
Result: Interleaved data on wire
```

**TcpStreamTransport solution:**
- `readMu` protects `bufio.Reader` state
- `writeMu` protects `bufio.Writer` state
- Separate mutexes allow concurrent `Send()` + `Receive()`

**TcpRequestTransport solution:**
- Single `mu` serializes entire request-response
- Simpler: one request at a time
- For concurrency: use multiple transports (connection pooling)

**Key insight:** Mutex held during **socket I/O**, not just buffer copy.
- This serializes writes on same transport
- But Server creates one transport per client/replica → no contention

## Deadline handling

**Episode 027 mistake:** Set deadline on `conn`, then call `framer.Write()`:
```go
conn.SetWriteDeadline(deadline)  // conn-level
framer.Write(payload)             // doesn't use that deadline properly
```

**Fixed:** Use Framer's timeout methods:
```go
framer.WriteWithDeadline(payload, deadline)  // Framer handles it
```

**Request timeout semantics:**
```go
// WRONG: 2x timeout in worst case
WriteWithTimeout(payload, timeout)  // up to timeout
ReadWithTimeout(timeout)            // up to timeout again

// RIGHT: shared deadline
deadline := time.Now().Add(timeout)
WriteWithDeadline(payload, deadline)
ReadWithDeadline(deadline)  // same deadline
```

## Lessons learned

**TCP fundamentals:**
- Full-duplex ≠ bidirectional request-response (needs multiplexing)
- Connection ownership: one goroutine per socket I/O
- Communication patterns are semantic: streaming vs request-response

**Abstraction timing:**
- Do it when cost is low (fresh knowledge, before complexity)
- Don't wait until forced by pain

**Thread safety:**
- Mutexes held during socket I/O (serializes writes)
- But per-connection transport = no contention
- bufio.Reader/Writer NOT thread-safe, need protection

**Deadline semantics:**
- Shared deadline for write+read (not 2x timeout)
- Use Framer methods, not raw conn.SetDeadline

## Migration: Server to transports

**Challenge:** Creating interfaces isn't enough. Server still uses `net.Conn + protocol.Framer` directly everywhere.

**Migration strategies:**

1. **Big bang replacement** - Rewrite everything at once
   - High risk, everything breaks together
   - Used when system is small or well-tested
   
2. **Adapter layer** - Wrap old types in new interface
   - Gradual migration, test each subsystem
   - Temporary complexity, adapter to remove later
   - Good for very large codebases with many teams

3. **Direct replacement with compiler guidance** - Change types, fix compilation errors
   - Type system shows every location needing changes
   - No technical debt from adapters
   - Works for single-team codebases
   - **Our choice**

**Why direct replacement:**
- Single codebase (not distributed teams)
- Strong type system catches all usages
- Compiler errors guide the migration
- Clean final state with no legacy adapters

**Migration plan:**

1. **Client connections:** Accept net.Conn → wrap immediately in TcpStreamTransport
2. **RequestContext:** Replace Conn/Framer fields with Transport
3. **replicaConn:** Replace `conn + framer` with `transport StreamTransport`
4. **Quorum reads:** Replace `map[net.Conn]*protocol.Framer` with `map[string]RequestTransport`
5. **Connection tracking:** Replace `map[net.Conn]struct{}` with `map[StreamTransport]struct{}`

**Key changes:**

```go
// Before: RequestContext with net.Conn
type RequestContext struct {
    Conn   net.Conn
    Framer *protocol.Framer
}

// After: Transport-based
type RequestContext struct {
    Transport StreamTransport
}

// Before: replicaConn with conn + framer
type replicaConn struct {
    conn   net.Conn
    framer *protocol.Framer
}

// After: transport-based
type replicaConn struct {
    transport StreamTransport
}
```

**Benefits achieved:**
- Server never imports TCP-specific types
- Protocol-agnostic: can add HTTP/2, QUIC without touching Server
- Testing: can create mock transports
- Clear boundaries: transport owns protocol, Server owns business logic

## Factory pattern for protocol independence

**Final violation:** Server still called `NewTcpStream()` and `DialTcpStream()` directly.

**Solution:** Transport factory
```go
// src/transport/factory.go
func NewStreamTransport(protocol string, conn net.Conn) StreamTransport
func DialStreamTransport(protocol, network, addr string, timeout) (StreamTransport, error)
func NewRequestTransport(protocol string, conn net.Conn) RequestTransport
func DialRequestTransport(protocol, network, addr string, timeout) (RequestTransport, error)
```

**Server.Options gains Protocol field:**
```go
type Options struct {
    Protocol  string // ProtocolTCP (default); future: QUIC, gRPC, etc.
    Network   string // NetworkTCP, NetworkTCP4, NetworkTCP6, NetworkUnix
    // ...
}
```

**Benefits:**
- Server completely protocol-agnostic
- Factory panics for unsupported protocols (fail-fast)
- Single point for adding new protocols
- Default to TCP when protocol empty

**Also moved Framer from protocol → transport package:**
- Framing is HOW to send (transport concern), not WHAT to send (protocol concern)
- Upper layers use transport interfaces, never see Framer
- Proper separation of concerns

## Testing

**Unit tests:** Transport layer, factory pattern, Framer behavior all tested independently.

**Integration tests:** All existing tests pass unchanged:
- Client connections work (acceptLoop → StreamTransport)
- Replication works (replicaConn with StreamTransport)
- Quorum reads work (RequestTransport fan-out)
- Timeouts respected (transport handles deadlines)
- epoch-test, quorum-test, and other scripts verify end-to-end functionality

**What's validated:**
- Thread safety: concurrent Send/Receive on same transport
- Deadline handling: shared deadline for request+response
- Protocol independence: Server has zero TCP-specific code
- Factory pattern: unsupported protocols panic at startup

## What's next

Episode 029: Service discovery (populate reachableNodes dynamically)  
Future: HTTP/2Transport, QuicTransport implementations

