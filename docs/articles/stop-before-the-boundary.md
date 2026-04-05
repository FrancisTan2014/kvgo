# Stop Before the Boundary and Prove What You Have

When I started building the Raft model in [(036b)](../036b-the-node-loop.md), I got stuck for days. I wanted a mechanism that was both correct and complete before writing a single line of code. The result was paralysis. I was exhausted, and for a while it felt like I might give up the project.

At that point, it was not even easy to say what was blocking me. My mind was full of message transfer, persistence, staleness, replication, and many other details. When too many topics arrive at once, there is no real topic anymore.

The turning point came when I returned to one claim from [036a](../036a-the-raft.md): the Raft model should be a pure state machine without side effects. If that claim was true, then there had to be a boundary between the model and the outside world. In the `etcd`-style design, that boundary is `Raft.Ready`.

Everyone can explain why `1+2=3` because `1+1=2` is already clear. I could not start because I had not found the `1+1=2` of the Raft model yet. Once I saw `Ready` as the first seam, the direction became clear. Raft did not need to solve transport, disk, and retries all at once. It only needed to accept input, change state, and expose output.

That was the first time I saw a logical boundary clearly. Before that, “boundary” felt like a vague engineering word. In 036b it became concrete: one side owns pure state transitions; the other side owns side effects. Once the concerns were split, each side became smaller, and the proof became smaller too.

The same lesson appears in `Advance`. In an `etcd`-like Raft model, `Advance` only moves the internal cursors after the current `Ready` batch has been processed. It does not need to know how entries were persisted or when messages were sent. Those questions belong to the other side of the seam.

The implementation and the test can both stay small:

```go
func (r *Raft) Advance() {
    // move internal progress forward
}

func TestAdvanceHidesProcessedReady(t *testing.T) {
    // before Advance: output is visible
    // after Advance: output is gone
}
```

That is the whole point: before `Advance`, the output is visible; after `Advance`, it is gone. No transport, no disk, no retries — just the boundary being proven.

Once I saw `Ready` as a real seam in 036b, the rest of the 036 series stopped looking like “build all of Raft” and started looking like “protect one boundary at a time.” 036c isolated persistence, 036d isolated visibility, 036s isolated transport, and 036u finally brought the model across the process boundary into a real server. The work became smaller not because the system was simple, but because each seam limited what had to be proven.

This also gave me a more concrete reason to write unit tests. For most of my 10+ years of work, unit tests often felt like a correctness ritual: good engineers do them, so I should do them too. Here the reason became sharper. We Chinese say "有的放矢" — shoot only when there is a target. The boundary is the target. Once the target is clear, the test has something specific to prove. Without that, testing easily drifts toward coverage for its own sake.

This lesson still feels simple, but it changed how I build. Confidence does not come from thinking about the whole system at once. It comes from proving one bounded thing, then standing on top of it. That is the first lesson the 036 series left in me, and I expect it to return whenever the system grows and the next seam needs to be found.