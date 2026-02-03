# The protocol
This is not a sabotage - it's infrastructure remediation.

The naive protocol design worked for simple `GET/PUT` operations, but replication exposed brittleness: subtle encoding bugs that took hours to debug but provided zero learning value for distributed systems concepts.

I've learned that Go doesn't favor OOP - it's designed for data-intensive applications where composition beats inheritance. This isn't because OOP is bad, but a deliberate design philosophy.

I'm applying OOP patterns I've learned through my career to refactor the protocol. The core idea is IoC (Inversion of Control): instead of maintaining a switch-case that gets modified every time we add a message type, I'll use a handler registry - one handler per message type. 

## Initial Skeleton

```
// General Protocol Structure
[4 bytes frameLen LE][4 bytes messageType][frameLen-4 bytes payload]
```

```Go

type RequestHandler interface {
    func Process(payload []byte) ([]byte, error)
}

// Register handlers on starting the server
handlers := map[int]RequestHandler

// Now the handling loop would become
f := protocol.NewConnFramer(conn)
for {
    var payload []byte
    payload, err = f.ReadWithTimeout(...)

    messageType = payload[0:4]
    handler := handlers[messageType]
    resp, err = handler.Process(payload[5:])
    f.WriteWithTimeout(resp, ...)
}

```

## The Problem: Stateful Connections

After discussing with AI, I realized the skeleton above assumes every request gets exactly one response. This breaks for replication:

**Concrete failure scenario:**
1. Replica sends PSYNC request
2. Handler returns FULLRESYNC response
3. Loop tries to read the next request
4. **Stuck**: Primary needs to send RDB stream (not waiting for a request), replica is waiting for snapshot data (not sending a request)

**The missing piece**: After PSYNC, the connection switches to **stream mode** - it's no longer request/response. The handler needs to "take over" the connection.

This mirrors ASP.NET Core's WebSocket upgrade pattern: middleware can write a response and return (normal flow), or switch protocols and block (WebSocket). Same concept applies here.

## Revised Design: RequestContext Pattern

**Wire format** (simplified from 4 bytes to 1 byte for message type):
```
[4 bytes frameLen LE][1 byte messageType][frameLen-1 bytes payload]
```

**Handler function type (delegate pattern):**
```go
// Using a function type instead of interface - more idiomatic Go for simple callbacks.
// This mirrors C# delegates: single-method contracts don't need interface ceremony.
type HandlerFunc func(*Server, *RequestContext) error

type RequestContext struct {
    Conn      net.Conn
    Framer    *protocol.Framer
    Payload   []byte
    takenOver bool
}
```

**Dispatch loop:**
```go
for {
    payload := f.Read()
    op := payload[0]
    
    handler := s.requestHandlers[protocol.Op(op)]
    if handler == nil {
        s.log().Error("unsupported request", "op", op)
        return
    }
    
    ctx := &RequestContext{
        Conn:    conn,
        Framer:  f,
        Payload: payload,  // Full payload - handlers use DecodeRequest()
    }
    
    if err := handler(s, ctx); err != nil {
        return err
    }
    
    if ctx.takenOver {
        return  // Handler owns connection now, exit loop
    }
}
```

## Handler Examples

**Normal handler (GET):**
```go
// Handlers are methods on *Server, registered as function values.
func (s *Server) handleGet(ctx *RequestContext) error {
    req, err := protocol.DecodeRequest(ctx.Payload)
    if err != nil {
        return err
    }
    
    key := string(req.Key)
    val, ok := s.db.Get(key)
    
    // ... build response ...
    return s.writeResponse(ctx.Framer, resp)
    // takenOver = false, dispatch loop continues
}
```

**Takeover handler (replication - future PSYNC):**
```go
func (s *Server) handleReplicate(ctx *RequestContext) error {
    // ... decode and validate ...
    
    rc := newReplicaConn(ctx.Conn, req.Seq, replid)
    s.mu.Lock()
    s.replicas[ctx.Conn] = rc
    s.mu.Unlock()
    
    // serveReplica takes over - blocks until replica disconnects
    s.serveReplica(rc)
    
    ctx.takenOver = true  // Signal: don't try to read next request
    return nil
}
```

**Control flow:**
1. Handler writes PSYNC response
2. Sets `takenOver = true`
3. Launches `serveReplicaStream` goroutine (non-blocking)
4. Returns immediately
5. Loop checks `takenOver`, exits
6. Connection stays open, owned by the goroutine

## Message Types

```go
const (
    MsgGet       = 1
    MsgPut       = 2
    MsgPSYNC     = 3  // Replication handshake
    MsgPing      = 4
    MsgPromote   = 5
    MsgReplicaOf = 6
)
```

## Implementation Plan

**Phase 1: Handler registry (no behavior change)** ✅
- Define `HandlerFunc` type and `RequestContext` (delegate over interface)
- Create handlers as `*Server` methods for existing ops
- Replace `handle()` switch with registry lookup
- **Validated**: All unit tests + partial-resync-test pass

**Phase 2: PSYNC message**
- Define `MsgPSYNC` with dedicated encode/decode
- Implement `PSYNCHandler` with `TakeOver()`
- Update `connectToPrimary()` to send MsgPSYNC
- Update `serveReplica` to use new flow
- Remove OpReplicate overloading
- **Validate**: `partial-resync-test.ps1` passes

**Phase 3: Cleanup**
- Remove StatusNoReply, StatusFullResync hacks
- Remove OpReplicate from message.go
- Simplify response validation
- **Commit**: "refactor(017): IoC protocol with connection takeover"

## Why This Works

**Extensibility**: Adding a new message type means implementing a handler and registering it in the map. No switch statement modification needed.

**Stateful connections**: Handlers can write a response and take ownership, or just write a response and return.

**Testability**: Mock `RequestContext` to test handlers in isolation.

**Familiar pattern**: Matches ASP.NET Core middleware, Express.js, and Go's http.Handler.

## Redis Comparison

Redis uses a single protocol (RESP) for everything, but changes connection mode via `client->flags`. They maintain a monolithic `struct client` with ~100 fields handling all connection types.

We're taking a different approach: explicit message types + handler registry + connection takeover pattern. Simpler for our scale, easier to reason about.