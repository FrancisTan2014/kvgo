# Sabotage
What happens if the primary node crashes?

## Architecture (Before Fix)

```
┌─────────────────────────────────────────────────────────────────┐
│                         Server.Start()                          │
│                              │                                  │
│                              ▼                                  │
│                     connectToPrimary()                          │
│                              │                                  │
│              ┌───────────────┴───────────────┐                  │
│              │                               │                  │
│              ▼                               ▼                  │
│      net.DialTimeout()              [handshake: CmdReplicate]   │
│              │                               │                  │
│              └───────────────┬───────────────┘                  │
│                              │                                  │
│                              ▼                                  │
│                      s.wg.Go(func() {                           │
│                          receiveFromPrimary(f)  ◄── goroutine   │
│                      })                                         │
│                              │                                  │
│                              ▼                                  │
│              ┌───────────────────────────────┐                  │
│              │   for {                       │                  │
│              │       payload := f.Read()     │ ◄── blocks here  │
│              │       if err != nil {         │                  │
│              │           log("stream closed")│                  │
│              │           return  ◄───────────┼── EXITS, GONE    │
│              │       }                       │                  │
│              │       // apply write          │                  │
│              │   }                           │                  │
│              └───────────────────────────────┘                  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘

Timeline when primary crashes:

     t0              t1                t2              t3
      │               │                 │               │
      ▼               ▼                 ▼               ▼
   Primary         Primary           Replica        Replica
   crashes         restarts          logs EOF       ... silence
                                                    (orphaned)
```

## What breaks
As the diagram shows, replicas become orphans when the primary dies.
A single primary is a single point of failure—cluster topologies address this, but that's beyond the scope of 018.

## The Fix
From the replica's perspective, the minimal fix is to retry connection indefinitely.

```
┌─────────────────────────────────────────────────────────────────┐
│                      replicationLoop(ctx)                       │
│                              │                                  │
│         ┌────────────────────┴────────────────────┐             │
│         ▼                                         │             │
│    ┌─────────┐                                    │             │
│    │ connect │──► fail ──► backoff ──► retry ────┘             │
│    └────┬────┘                                                  │
│         │ success                                               │
│         ▼                                                       │
│    receiveFromPrimary(ctx, f)                                   │
│         │                                                       │
│         ▼                                                       │
│    stream closes (EOF) ──────────────────────────►──────────┘   │
│                                                    (loop back)  │
└─────────────────────────────────────────────────────────────────┘
```

### Subtle Issue: Relocate Race

What if someone calls `REPLICAOF new-primary` while the loop is running?

**Problem:** Closing `s.primary` kicks the old loop out, but it immediately tries to reconnect—possibly to the old address, or racing with the new loop.

**Solution:** Use `context.Context` for cancellation.

```go
type Server struct {
    replCtx    context.Context
    replCancel context.CancelFunc
}

func (s *Server) startReplicationLoop() {
    if s.replCancel != nil {
        s.replCancel()  // Cancel old loop
    }
    s.replCtx, s.replCancel = context.WithCancel(context.Background())
    ctx := s.replCtx
    
    s.wg.Go(func() {
        s.replicationLoop(ctx)
    })
}
```

The context flows through: `replicationLoop` → `connectToPrimary` → `receiveFromPrimary`. Any level can check `ctx.Done()` and exit cleanly.

**Four exit conditions:**
1. `ctx.Done()` — relocate, shutdown, or explicit cancel
2. `!s.isReplica` — promoted to primary
3. Stream EOF — primary died, loop retries
4. `Shutdown()` — cancels context and closes primary connection

## Result
`partial-resync-test.ps1` Scenario 2 now passes:
- Replica detects primary death
- Retries connection (100ms interval)
- Reconnects when primary restarts
- Full resync (new replid)