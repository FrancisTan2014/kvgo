# Sabotage

A client sends a `PUT` with the `--quorum` flag. The primary saves the data locally, fans out to replicas, waits for majority acknowledgment, then replies success. Happy path.

**What if the quorum round fails after the local write?**

The server tells the client the write failed. But the data is already in the primary's WAL. If the client reads immediately, it gets the value it was told wasn't saved.

That's a fake quorum write. The client can't trust the response.

## The problem

The root cause isn't the fan-out logic. It's the ordering: local write first, consensus second. If consensus fails, the write already happened. There's no way to untangle them.

The only fix is to flip the order: consensus first, local write after. A write is durable only when a majority has it. The state machine applies it after. The local write and the consensus round can never be split — because the log entry *is* the write.

That's not a feature. That's an architecture.

## Deciding to implement Raft

The current quorum write bolts consensus onto a system that wasn't built for it. Every fix creates three new edge cases. Episodes 001–035 built a Redis-like store. The honest conclusion: to make `kv-go` correct, it needs to become an etcd-like system.

It's hard to commit to a full rewrite even in a learning project. But the alternative is worse: keep patching a design that can't be made correct. I decide to bring Raft in.

`kv-go` is no longer a Redis-like database from this point. Consensus isn't a feature to add — it's an identity change. Once the log is the center, everything else moves to the edge. The KV store becomes a state machine that the log drives, not the other way around.

## Reading up

`etcd` implements Raft as a pure state machine — zero I/O, zero network calls inside the algorithm. All side effects happen outside: the application reads a `Ready` struct, writes to disk, sends messages, applies entries to the state machine, then calls `Advance()`. The algorithm itself is a pure function.

The benefit is obvious after 35 episodes of subtle race conditions: if the algorithm has no I/O, you can test it exhaustively. Any message sequence, deterministic output, no goroutines, no timing, no network.

I'm not going to use etcd's Raft package. I want to build it from scratch.

After reading the Raft paper and walking through etcd's implementation, I find myself unable to start. Details explode every time I try. I've been sitting on this for days.

That's the perfectionism trap. I know what it feels like by now. The way out is to start with something concrete, even if it's wrong.

## Starting: what is the subject?

A Raft cluster is a set of nodes that maintain a replicated log. Each node runs the same algorithm. The subject — the central object — is a single node.

```go
type Raft struct{}
```

A node must be identifiable. Without an ID, nodes can't address messages to each other.

```go
type Raft struct {
    ID uint64
}
```

Here are all the states the Raft paper says each node maintains:

```go
type Raft struct {
    ID uint64

    // Persistent state — must survive crashes
    Term     uint64
    VotedFor uint64
    Log      []Entry

    // Volatile state — all nodes
    CommitIndex uint64
    LastApplied uint64

    // Volatile state — leaders only
    NextIndex  []uint64
    MatchIndex []uint64
}
```

And I'm stuck again. Listing the state didn't help — it just moved the explosion inside the struct. The struct isn't the entry point.

Raft is a state machine. A state machine is state plus transitions. The struct holds the state. But state sitting in memory doesn't do anything — it has no causality. The transitions are what give it life: message in, state changes, output out. That's where to start.

The entry point is the *loop* — the thing that receives messages and produces output. Everything the struct holds is dead data without it. That's what episode 036b builds.

## References

1. [The Raft paper](https://raft.github.io/raft.pdf)
2. [etcd raft library](https://github.com/etcd-io/etcd/tree/main/raft)
3. [raftexample](https://github.com/etcd-io/etcd/tree/main/contrib/raftexample)