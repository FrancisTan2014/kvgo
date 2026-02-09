# Sabotage
Client sends PUT to replica instead of primary.

## Current behavior
Replica rejects with generic StatusError.

## Problems
1. Client doesn't know WHY it failed (wrong server? disk full?)
2. Client doesn't know WHERE to retry (must guess)
3. Failover breaks: client keeps hitting dead primary

## Options
**Reject (current):** Simple but poor UX
**Redirect:** Add StatusReadOnly + primary address  
**Proxy:** Replica forwards to primary (adds latency)

## Decision
Implement **StatusReadOnly + primary address** (redirect pattern).

**Why:**
- Client can immediately retry correct server (no guessing)
- Survives failover (new primary address propagated)
- Matches Redis READONLY semantics
- Bounded change: protocol + one handler modification

**What changes:**
1. Protocol: Add `StatusReadOnly` status code
2. Protocol: Add primary address field to Response
3. Server: Replica returns primary address when rejecting writes
4. Client: Handle StatusReadOnly by retrying to primary address

**Tradeoff:**
- ✅ Better availability (no retry storm)
- ✅ Clear error semantics (actionable)
- ❌ Protocol breaking change
- ❌ Extra network hop (client → replica → client → primary)

**Compared to proxy:**
- Redirect: Client retries (1 extra hop)  
- Proxy: Replica forwards (1 extra hop + replica becomes SPOF)

Redirect is simpler and doesn't make replica a single point of failure.

---

## Implementation

**Protocol (message.go):**
```go
const (
    StatusReadOnly Status = 3 // Replica cannot accept writes; Value contains primary address
)
// StatusReadOnly responses carry primary address in Value field (reuses existing mechanism)
```

**Server (handler_put.go):**
```go
if s.isReplica {
    s.log().Warn("PUT rejected on replica", "key", key)
    return s.writeResponse(ctx.Framer, protocol.Response{
        Status: protocol.StatusReadOnly,
        Value:  []byte(s.opts.ReplicaOf), // Primary address for client redirect
    })
}
```

**Client (kv-cli/main.go):**
```go
case protocol.StatusReadOnly:
    primaryAddr := string(resp.Value)
    fmt.Printf("(replica, redirecting to primary: %s)\n", primaryAddr)
    return doPutToPrimary(key, value, primaryAddr, timeout)
```

Client automatically connects to primary and retries.