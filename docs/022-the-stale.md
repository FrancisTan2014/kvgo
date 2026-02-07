# Sabotage

Replicas keep serving reads even when they're partitioned.

## Thinking
What breaks?
This behavior gives our clients the illusion of an availability guarantee.
But `kv-go` has been implicitly designed as a `CP` system.
We terminate the `Server` when integrity cannot be guaranteed.

Thus, nothing breaks. We just need to state the architecture more clearly:
> We guarantee consistency when facing network partition.

**English note:** "gives X an illusion" → "gives X THE illusion" (use "the" for specific things)

When a replica receives a read request (can replicas serve writes? out of scope, maybe a future sabotage),
the replica must be aware of its state before executing the request.
Given the architecture, the baseline is that we don't serve any requests if consistency cannot be guaranteed.
This baseline serves as an anchor that pulls me back when I fall into the massive state space.

**English note:** "With respect to" means "concerning" but sounds overly formal here. Use "Given" or "Based on."

With the baseline, we can make the top layer decision as:
```go
if replica is healthy
    serve requests
else
    reject requests
    OR
    seek flexible solution?
```

What can a replica do at this point?
What does unhealthy mean in a distributed system?
Aha! Recall our name `the stale`; I have the second decision layer:
```go
if replica loses connection to all nodes
    reject requests // consistency not guaranteed
else
    seek consistency path
```

What about waiting for a duration to see if the replica can recover the connection?
No, it's not a good idea; the traffic may be heavy. It introduces complexity, and, even worse, it makes the system fragile.
Additionally, these continuing attempts are only trying to make our system flexible; they are not things we must do.

**English note:** "How about" suggests invitation/suggestion. "What about" is better for evaluating options.

I find it difficult to think about such problems since I've never faced them before.
We cannot guarantee consistency as a replica.
Wait!
Consistency is not equal to integrity!
It reminds me of another term: eventual consistency.
By following this idea, we can return what we have along with a flag that indicates out-of-date risk.
We can also design a policy to let operators decide what level of guarantee we provide.
A naive policy may be like:
```go
type ConsistencyPolicy struct {
    GuaranteeLevel                  int  // absolute or eventual or others
    ForwardToPrimaryOnDisconnection bool // tell the clients to switch the server
    ...
}
```

No, we can't. I'm starting to fall back into treating consistency as a feature.
Recall the `blood` metaphor in [010-the-consistency](010-the-consistency.md); every decision must respect it.

**English note:** "fall back to treat" → "fall back into treating" (gerund after preposition)

I think I've reached the boundary I can imagine for handling the current connection.
A replica should do something to reconnect to its primary in the meantime.
For now, let's make a naive decision: retry forever until reconnected.

I stop here to escape from the open thinking, for instance, developing a path from thinking about
the relationship between the replica and its reachable nodes.
I don't make arbitrary assumptions since I'm learning distributed systems instead of doing research.

---

## Coach guideline
Consistency levels (the names, compressed the problems and solutions altogether):
- **Linearizability**: Reads always see the most recent write (strongest)
- **Sequential consistency**: All nodes see same order, but not necessarily "most recent"
- **Causal consistency**: If A happened-before B, everyone sees A before B
- **Eventual consistency**: Eventually converges, no ordering guarantee

Replica failure modes:

**Typo fixed:** faliure → failure
- **Network partition**: Replica can't reach primary, but both are alive
- **Primary crash**: Primary is dead, replica is orphaned
- **Replica crash**: Replica restarts, lost in-memory state

Design outlining:
- **Define your actual consistency guarantee**: Right now, can a healthy replica lag behind primary by 100ms? If yes, you're not linearizable - you're sequential at best.
- **Failure detection**: How does replica know if it's partitioned vs primary is dead? (Hint: it can't know for certain - this is the FLP impossibility in disguise)
- **Client contract**: What does your protocol return when replica rejects a read? How does client know to retry?
- **Partition tolerance**: If replica serves stale reads, what's the worst thing that breaks in your system?


**The hard question:**

You run a bank on kv-go. Client reads account balance from replica. Replica is partitioned but doesn't know yet (last heartbeat was 5s ago, heartbeat interval is 10s).

-   If you serve the read: Balance might be stale, client acts on wrong data
-   If you reject the read: Client sees downtime, complains about availability

Which failure mode is worse for a bank? Which one would you choose for a social media feed?

This is CAP in concrete terms. What's your answer?

---

## Read up - Consistency — One-screen Mental Model

Consistency = the illusion a distributed system gives about replicated state.

Stronger consistency ⇒ more coordination ⇒ higher latency / lower availability.

Core models:
- **Linearizable**: one real-time global timeline (max safety, max coordination)
- **Causal**: effects never precede causes (no global time)
- **Eventual**: replicas converge if writes stop (max availability)

Practice:
- Use **session guarantees** to make weak models usable.

Design rule:
> Pick the weakest model that preserves the invariants your system cannot violate.

---

## Read up - FLP
FLP refers to the **impossibility result proved by Fischer, Lynch, and Paterson**.

> In a fully asynchronous distributed system,
> with even one possible crash failure,
> no deterministic algorithm can guarantee consensus.

---

## Keep thinking
That's it, the coaching style I want, challenging my brain and making me feel uncomfortable.

**English note:** "discomfortable" is not a word. Use "uncomfortable."

I've read the unspoken words in the feedback: I'm still thinking from the feature implementation perspective.
Falling into code details "smoothly" without awareness.

**English note:** "unsaying" is not standard. Use "unspoken" or "implicit."

Let me restart from the hard question.
I must make a solid choice on the consistency level and stop wavering.
As a learning project, I choose **Eventual Consistency**.
Hence, I correct my earlier wrong claim: `kv-go` is an AP system.

**English note:** "waving in the wind" is wrong. Use "wavering" (changing opinion) or "flip-flopping."

Therefore, the answer to the hard question is: `kv-go` can only be used to build a bank system when you don't distribute it.

**English note:** "After that" suggests sequence in time. "Therefore" shows logical consequence.

Here is an adjusted decision tree:
```go
if replica is up-to-date
    return whatever it holds
else
    if primary is not reachable after a while
        return whatever it holds
            + staleness flag
    else // maybe in SYNC/PSYNC
        return retry navigation to the primary
```

---

## Coach feedback
**What I Want You To Reason Through**

Don't write code yet. Answer these:

1. Current state: Right now, can a healthy replica lag behind primary? By how much?

2. Read-your-own-writes: Client writes to primary, immediately reads from replica. Can it fail to see its own write?

3. Partial partition: Primary + 5 replicas healthy, 5 replicas partitioned. Should the 5 partitioned replicas go offline? Does that help?

4. Staleness bound: If you serve stale reads, what's the maximum staleness? (Last heartbeat time? Replica knows this.)

5. Client semantics: Can clients tolerate stale reads? Do they even know they're reading from a replica?

Once you reason through these, you'll see: "CP system" isn't a decision you make once. It's a tradeoff you navigate continuously.

**What's your answer to question #2?** This will reveal what guarantee you're actually providing right now.

--- 

## Keep thinking
Q1: Yes. Approximately, it's double `fsync` cost + network latency.
Q2: Yes. We provide **Eventual Consistency** guarantee.
Q3: No. We serve with staleness flag.
Q4: New point to me, I think last heartbeat time is reasonable.
Q5: I was confused by the question. Don't clients make decisions based on the component's constraints?

---

## Coach feedback
Progressed a little but stayed on the surface level.

**What You Need To Reason Through Now**
1. Session guarantees: Do you provide read-your-writes? How?

2. Staleness bound: Last heartbeat time works, but what if heartbeat is 1 hour old? Still serve?

3. Client contract: Does client know about replicas? How does retry work?

4. Invariant analysis: Pick one domain (bank, social feed, shopping cart) and trace:
    - What operations exist (read, write, transfer)
    - What invariants must hold (balance >= 0)
    - How eventual consistency breaks them
    - What's the recovery path

Do this for ONE domain. Make it concrete. Not "eventual consistency means stale reads" - show me the actual sequence of operations that breaks.

Start with the bank example I gave above. Trace through what happens when replicas converge. How does the system detect balance=-60? What does it do?

---

## Keep thinking
Feeling painful in the brain.

1. How can a `kv` server based on TCP guarantee sessions? It seems to be the responsibility of some dispatcher like a load balancer.

**Grammar note:** "guarantee session" → "guarantee sessions" (plural or "a session")
2. Okay, we need a concrete duration, can be configurable through options.
3. Add metadata information to the response, and bring it back to the server when retry is needed, maybe handled by some dispatcher?
4. Social feed tolerates stale reads the most. I choose shopping cart:
    - Add/Remove from the cart, and inventory count updates.
    - Ah, it's not that simple. I cannot come up with an invariant; real computation happens when creating an order.
    - Eventual consistency seems to never break them? Maybe the inventory count would be affected.
    - I'm losing my way.

**English note:** "staleness read" → "stale reads" (noun form) 
**Expression:** "I lose my mind" is too strong. Use "I'm losing my way" or "I'm lost."

In the bank example, when replicas converge:
1. The server is still accepting writes—say 10 new writes completed when the first write finally applied to replicas.
2. How does the system detect balance=-60? The primary knows it first, but replicas need time to sync.
3. I've lost my direction; what is the question really asking?

**English note:** "say 10 new wites" → use em dash (—) or colon, and fix typo "wites" → "writes"  
**Grammar:** "I lost my direction, what's" → "I've lost my direction; what is" (semicolon between related clauses)

---

## Conclusion (Coach)

**What you actually learned in 022:**

Consistency is not a feature you implement—it's a tradeoff you navigate based on the invariants your application must preserve.

You started thinking "kv-go is CP, reject stale reads." You ended by realizing: even "healthy" replicas are stale due to replication lag. Guarantees cost coordination, which costs latency and availability.

**Core insights:**
1. **Guarantees have costs:** Linearizability requires synchronous replication (high latency). Eventual consistency requires low coordination (high availability). Real systems pick intermediate points.

2. **Failure detection is ambiguous:** When a replica loses heartbeat, it cannot distinguish "network partition" from "primary crashed." No algorithm can tell "slow" from "crashed" (FLP impossibility).

3. **Session guarantees bridge the gap:** Eventual consistency alone is too weak for most applications. Read-your-writes and monotonic reads make it usable in practice.

4. **Client contract matters:** Does the client know it's talking to a replica? Can it tolerate stale reads? These are system-level design decisions, not implementation details.

**Where this sits on the map:**
```
You are here:
├─ Replication mechanics ✅ (009)
├─ Basic failure recovery ✅ (018-orphan reconnect)
└─ Consistency tradeoffs ⚡ FELT, NOT YET INTERNALIZED
    ↓
    Next: Explore more failure modes before solving problems you haven't encountered
```

**Decision deferred:**

For now, kv-go replicas serve reads whenever alive, regardless of staleness. This provides eventual consistency—replicas converge if writes stop, but no ordering or recency guarantees.

The open questions (do clients know about replicas? what session guarantees? how to bound staleness?) don't have universal answers. The right choice depends on what invariants the application needs.

You haven't felt enough failure modes yet to know what guarantees you actually need. Move forward to encounter more pain points.

**Note:** You already explored split-brain (two primaries) in [015-the-split](015-the-split.md) with epoch-based recovery. The next unexplored territory is likely conflict resolution or write handling on replicas.
