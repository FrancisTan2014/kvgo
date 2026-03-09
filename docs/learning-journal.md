# Learning Journal

## 2026-03-03 — Reading with Intent

### The idea

Treat outside information (books, blogs, OSS) as material to organize *your* idea — not as authority to obey. The most important thing is: take an idea first, then read.

### The pattern

1. **Form a hypothesis** — even if wrong, you need an anchor.
2. **Read to test it** — does etcd agree? Does the Raft paper contradict? What did I miss?
3. **Revise or confirm** — update your model with evidence, not someone else's conclusion.

### What happens without an idea first

- **`Cargo` cult:** copy etcd's design without understanding why. Can't debug when your context differs.
- **Paralysis:** read three conflicting approaches, can't choose because there's no anchor.

### What happened in 036b

I started with "I think Ready/Advance is a handshake." Hit a wall with CommitTo ordering. Read etcd to find evidence (nil-gating pattern, commitTo on raftLog). Revised my model. The reading was surgical because I had a question. If I'd read node.go top-to-bottom first, I'd have memorized structure but missed the *why*.

### On source code reading

Enter with a concrete purpose. Find the evidence. Stop. Return to your own workspace. A giant codebase is a trap if you browse without intent.

### The cost of being wrong

"I think CommitTo is an external API" → read etcd → wrong, it's internal to Step → revised in 5 minutes. The cost of the wrong hypothesis was near zero. The learning was permanent.

### Literature vs. technical reading

Fiction rewards open-ended exploration — you don't know what matters until the end. Technical material rewards **adversarial reading** — enter with a claim and look for what breaks it.

General reading suits literature. Targeted reading with exit criteria suits engineering.

---

### The week that almost broke me

1. Asked AI to write a massive design doc. Prepared to implement the whole rewrite in one shot.
2. Felt afraid of the illusion of learning. Started reading the Raft paper and `etcd/raft` end-to-end — got nothing but tired.
3. After 4–5 days without a single commit, started 036a. Claimed: I must start, immediately.
4. Today, finally reached the calm path. ~80 lines of code, ~80 lines of tests, every line proven.

The pattern: panic → overreach → exhaustion → forced start → small steps → calm.

Next time, skip to step 3.

---

## 2026-03-09 — Hidden Boundaries Need Explicit Names

### The lesson

I got stuck in 036d because I treated a temporary seam as a draft of the final architecture. That created a discontinuity in my head: 036b built a concrete loop, 036c built concrete storage, and 036d suddenly sounded like "invent a fake interface." The problem was not the code. The problem was that the boundary was hidden.

Once the boundary was named, the episode became coherent again. 036d is not about integrating Raft into the full server yet. It is about proving one visibility invariant: only committed entries become visible to the state machine, and failed apply must not advance progress.

### The heuristic

When an episode starts to feel fake or disconnected, ask:

1. **What invariant are we proving right now?**
2. **What is the smallest seam that makes that invariant testable?**
3. **Am I building the final architecture, or only a temporary boundary that reveals the invariant?**

If I cannot answer those three questions, I am probably drifting into draft-level thinking.

### What changed

- 036b built the execution seam
- 036c built the persistence seam
- 036d builds the visibility seam

The continuity was there, but I could not see it because the boundary had not been stated explicitly.

### What to remember next time

Not every concrete thing is a permanent subsystem. Sometimes the concrete thing is a boundary. If the boundary is hidden, name it first. That often removes the confusion faster than reading more code.

---

## 2026-03-09 — Throughline: Seeing Boundaries

### The claim

The ability to see and name boundaries is a key switch between feature-implementing programmers and system-level programmers.

### Why this matters

Feature work often feels local: add behavior, wire it up, move on. System work is different. The hardest part is often not the feature itself, but identifying the boundary that makes the feature safe, testable, and coherent.

Once the boundary is visible, several things become easier at once:

- the invariant becomes explicit
- the test target becomes smaller
- the design doc stops drifting into speculation
- the architecture regains continuity

### What 036 taught me

- 036b: the execution boundary (`Ready` / `Advance`)
- 036c: the persistence boundary (`Storage` / WAL)
- 036d: the visibility boundary (committed entries / applied state)

The episodes looked different on the surface, but the throughline was the same: each one became clear only after the hidden boundary was named.

### What to remember

When I feel lost in system design, I should stop asking, "What feature am I building?" and ask, "What boundary am I failing to see?"

---

## 2026-03-09 — Why Unit Tests Feel Hard and Verbose

### The claim

Unit tests feel hard when the invariant is unclear, and they feel verbose when the goal collapses into coverage instead of protection.

### Why they feel hard

If I cannot say what must never break, I do not know what the test is trying to protect. Then every test case feels arbitrary. I keep adding setup, mocks, and assertions without confidence because the real target is still hidden.

### Why they feel verbose

When the target becomes "cover more lines," the test stops being shaped by an invariant. It starts stretching to touch code rather than to catch a specific failure. That is where the nonsense feeling comes from.

### A better testing heuristic

1. **What invariant am I protecting?**
2. **What is the smallest seam where it can fail?**
3. **What is the smallest test that would catch that failure?**

Coverage can still be a useful signal, but it should come after the invariant is clear, not before.

### Connection to the throughline

The same skill shows up again: seeing hidden boundaries. In system design, the boundary makes the architecture coherent. In testing, the seam makes the invariant testable.

---
