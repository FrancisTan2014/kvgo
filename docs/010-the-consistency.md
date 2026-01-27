# Sabotage
What if a replica loses connection mid-flight?

## Analysis
We implicitly treat the network as a reliable resource, just like a system call. Unfortunately, it's not.

Once that illusion breaks, the problem surfaces naturally: how do we guarantee consistency? Looking back at our design, we simply drop unavailable replicas when the connection fails. If a replica reconnects seconds later, it has lost every write that happened during the disconnect — forever. That's a gaping hole; consistency is not guaranteed.

For certain distributed systems (say, a cache), this isn't wrong — it's an extreme choice. Recall the [mental model](004-the-flush-point.md) we used when deciding when to flush buffers: picking a point between two extremes. The idea is the same here, but the environment has changed.

That leads me to the common approaches our industry chooses:

```
        Strong ──────────────────────────────────────► Weak
        
        Linearizability → Sequential → Causal → Eventual
        (Spanner)                                (Redis, Dynamo)
```

My first instinct was to build a policy-based solution letting users pick their consistency level. I quickly realized I was wrong.

After reading more, I understood:

> Consistency design begins not with guarantees, but with losses you are willing to accept.

**I was wrong because I treated consistency as a feature. In reality, it's the blood that flows through your entire system — every design decision either preserves or sacrifices it.**

From this perspective:
- **Redis** sacrifices global consistency to preserve simplicity and speed.
- **Dynamo** sacrifices global ordering to preserve availability.
- **Spanner** sacrifices latency and simplicity to preserve linearizability.

So the question becomes: what sacrifice does `kv-go` accept?

At this stage, I'll follow Redis: **accept eventual consistency**, keep the system simple, and acknowledge that replicas may lag or miss writes. This is honest for a learning project — we're exploring trade-offs, not building a bank.

## What to do

1. **Document our consistency model** — add a comment in `server.go` stating that replication is async, fire-and-forget, with no delivery guarantees
2. **Add a sequence number to writes** — primary assigns a monotonic `seq` to each PUT; replicas log the last applied seq (helps debugging gaps)
3. **Log replica lag on reconnect** — when a replica connects, primary could log "replica last saw seq X, we're at Y" (visibility into the gap)
4. **Stretch goal: basic catch-up** — primary keeps last N writes in a ring buffer; on reconnect, replay missed writes if within the buffer

For now, steps 1–3 give us visibility without complexity. Step 4 is optional — it shows the path toward stronger guarantees without fully walking it.

## Reflection

The real lesson here isn't code — it's that **consistency is a system-wide property, not a module**. You can't bolt it on later. Every choice we made (async replication, no acks, fire-and-forget) already decided our consistency model. This doc just makes it explicit.
