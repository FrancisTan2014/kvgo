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

## 2026-03-10 — Three Skills Behind Clear Design Docs

### The claim

Clear technical prose is usually a downstream effect of three design skills:

1. **Name one invariant**
2. **Name one boundary**
3. **Cut everything else out**

When those three skills are weak, the doc feels muddy. When they are sharp, the prose becomes simple almost by itself.

### 1. Name one invariant

The first question is not, "What am I building?" It is, "What must remain true?"

An invariant compresses the episode into one sentence. Without it, the doc starts collecting related ideas instead of protecting one truth. That is when the writing turns broad, repetitive, or speculative.

The practical test:

- Can I summarize the episode in one sentence?
- Does every paragraph strengthen that same sentence?

If not, the invariant is still blurry.

### 2. Name one boundary

Once the invariant is visible, the next question is: where does it become testable?

The boundary is the seam where the invariant can fail in a controllable way. Naming it prevents the design from drifting into the whole system at once.

Examples from the Raft series:

- 036b: execution boundary
- 036c: persistence boundary
- 036d: visibility boundary
- 036e: compaction boundary

If the boundary is unnamed, the doc usually compensates with too much explanation.

### 3. Cut everything else out

After the invariant and boundary are named, most of the remaining work is removal.

This is the hard part emotionally. Extra context feels helpful, but it often hides uncertainty. Future episodes, implementation details, and neighboring concerns should be removed unless they directly support the current invariant.

This is not oversimplification. It is scope discipline.

### The methodology

When writing a design doc, use this sequence:

1. **State the invariant in one sentence.**
2. **State the boundary that makes it testable.**
3. **List what is explicitly out of scope.**
4. **Delete any paragraph that does not strengthen 1–3.**

If the prose still feels muddy, the problem is usually not grammar. The problem is that one of those three design steps is still incomplete.

### What to remember

Beautiful technical prose is rarely produced by decorative writing. It usually appears after the idea has been compressed enough. First compress the design. Then the prose has room to become clear.

---

## 2026-03-10 — Design Moves Back and Forth Through Proven Boundaries

### The claim

The most valuable thing I am learning from `kv-go` is not a specific Raft API or storage trick. It is a deeper model of software design: good architecture is not built by walking only from the ground up or only from the top down. It stabilizes by moving back and forth until the boundaries are proved.

### The old naive model

I used to think design should happen in one direction:

- either start from low-level pieces and build upward
- or start from the big picture and push downward

Both views contain part of the truth, but neither is enough on its own.

### The better model

Real design moves in both directions.

1. Start with a pressure, invariant, or architectural guess.
2. Build enough to touch the problem directly.
3. Notice where the model becomes awkward or unstable.
4. Name the boundary that resolves that pressure.
5. Prove the boundary with tests, failures, or implementation constraints.
6. Reuse that proved boundary as a trustworthy abstraction barrier at the next level up.

That is the key: a boundary becomes reusable only after it has been earned.

### What 036e made visible

036e did not just teach me compaction. It exposed several deeper moves:

- `SnapshotMeta` appeared because the compaction boundary needed a name
- `FirstIndex` and `LastIndex` became methods only after stored fields started overlapping awkwardly
- the no-gap rule after the snapshot boundary appeared only after replay semantics were made concrete

None of those abstractions should have been imported early just because etcd has them. They became legitimate only when the problem forced them into view.

### Why this feels important

This is the first time software design feels less like arranging ideas and more like discovering stable joints. Once a boundary has been proved, higher-level structure no longer feels forced together. The architecture begins to hold naturally because each part leans on a boundary that already survived contact with reality.

### What to remember next time

- Do not force design into one direction.
- Go down to touch the problem, then back up to name the boundary.
- Borrow abstractions cautiously.
- Add a piece only when the problem has earned it.
- When discomfort appears, ask the question immediately. That discomfort often marks the exact place where the next boundary is hiding.

---

## 2026-03-10 — Real Work Does Not End When the Doc Is Written

### The claim

Real work is not a straight pipeline of `doc done -> code -> code done -> publish`. Thinking continues through all of it. A design doc is not a contract frozen in advance. It is an anchor that helps the mind stay oriented while reality pushes back.

### What I felt during 036e

I could have finished the code earlier if I had stopped asking questions. But the more important thing was that each question made 036e more solid:

- the compaction boundary became explicit as `SnapshotMeta`
- `FirstIndex` and `LastIndex` shifted from fields to derived methods
- the no-gap rule after the snapshot boundary became visible as a fatal validity check

Those were not distractions from implementation. They were implementation doing its real job: forcing the design to become more truthful.

### The better model of work

1. Write a design doc to establish an initial invariant and boundary.
2. Start implementing before the ideas feel perfectly complete.
3. Ask questions whenever the implementation creates real discomfort.
4. Let the doc change when reality exposes a better shape.
5. Keep repeating until the design stops feeling approximate and starts feeling solid.

That is not indecision. That is the design being tested by contact with the real problem.

### Why this matters

If I treat the doc as fixed too early, I start protecting the document instead of protecting the invariant. Then implementation becomes mechanical and the deeper learning disappears.

If I treat the doc as an anchor instead, I can keep my direction without pretending the first version was complete. The doc holds the throughline while the implementation sharpens the truth.

### What to remember next time

Finishing code quickly is not the only measure of progress. Sometimes the highest-value work is the questioning that makes the design trustworthy before it hardens.

---

## 2026-03-11 — Truth Before Shape

### The claim

Elegant shape is dangerous when it arrives before semantic truth.

### What happened in 036f

I saw that etcd has one `Step()` and copied the surface shape too early. That made leader proposal and follower append look more uniform than they really are.

The missing truth was authority:

- leader originates proposals
- follower accepts replication
- candidate drops proposals

One entry path in code did not mean one meaning in the state machine.

### The lesson

Do not choose structure by symmetry first. Choose it by invariant and authority first.

Local guards do not automatically compose into global clarity. Once multiple behaviors share state and transitions, composition starts forcing the real shape of the design.

That is why elegance is often late. It may look intuitive in mature code, but it is usually the result of refactoring after the truth has already been paid for.

Bad order:

1. see a clean shape
2. imitate it
3. discover the semantics were different

Better order:

1. name the invariant
2. name who is allowed to cause the transition
3. let the structure follow

### What to remember next time

- unified entry does not mean uniform behavior
- similar code paths may carry different authority
- shape should compress truth, not hide it

---

## 2026-03-12 — Provisional Shapes Are Normal

Some shapes appear early because shared paths already need a wider container. That does not make them final.

Example: `Message` already carries `[]Entry` even though 036g only needs one-entry append semantics. The wider slice shape leaked in early because the same append path will later carry one entry, many entries, and catch-up entries.

The real job is to keep the current invariant honest even if part of the future shape leaks in early. If later evidence proves the shape wrong, change it.

---

## 2026-03-12 — Coding Exposes Missing Ownership

The design looked locally coherent until code reached `To:` in `Propose`. That was the moment the missing truth appeared: who owns routing?

The fix was not to widen `Propose(data)`. The fix was to put peer knowledge back inside `Raft`, where outbound replication decisions belong.

---

## 2026-03-12 — Surface Beauty, Hidden Truth

Art shocks twice. First through color and form. Then through the tiny details behind them.

Software is similar. A mature design looks beautiful on the surface, but the deeper shock comes from the small details quietly carrying the truth.

---

## 2026-03-14 — A Small Design Doc Can Still Tell a Real Story

### The lesson

036i showed me that a design doc does not need to choose between "real-world story" and "small provable step." It can do both, if the story is used only to motivate one bounded invariant.

### The pattern

The writing shape was:

1. Start with a real failure pressure
2. State that the full coordination story is out of scope
3. Extract one local rule from that pressure
4. Show the smallest design consequence
5. End by naming the invariant explicitly

### Why this worked

Without the motivating race, log freshness would feel arbitrary. Without the bounded rule, the doc would drift into full election design. The useful middle path was: tell just enough story to make the invariant feel necessary, then stop.

### What to remember

Professional technical writing does not need to sound large. One real pressure, one bounded rule, one small design consequence, and one explicit invariant are enough.

This structure works for me because it forces thinking within boundaries and builds upper layers only on proved invariants. I may not be able to apply it perfectly at work under time pressure, but it has already changed how I read code, shape designs, and write technical prose.

---

## 2026-03-16 — Human Reasoning Needs Small State Spaces

### The claim

Human reasoning works only in a small state space. Stable architecture appears only when upper layers stand on proved boundaries.

### Why this matters

When a design feels hard because there are too many possible failure paths, the real problem is usually not lack of effort. The real problem is that the state space is too large to hold clearly.

That is why invariants matter. A good invariant removes many states at once. A proved boundary preserves that simplification so the next layer can safely build on top of it.

### The architectural lesson

Without a proved boundary, the upper layer keeps re-opening old questions. It cannot trust what is below.

With a proved boundary, many earlier ambiguities stop leaking upward. The architecture becomes calmer because the next layer is built on something already earned.

### What to remember

If the state space feels too large, stop building upward.

1. find the missing invariant
2. prove it at a real boundary
3. then continue from the smaller state space

Small state spaces make reasoning possible. Proved boundaries make architecture stable.

---

## 2026-03-16 — Architectural Progress Is Often Invisible Until a Boundary Holds

Architectural work can feel slow because much of the progress is not visible as features.

But the real progress is this: a boundary becomes clear, proved, and safe for the next layer to trust.

Feature progress is easy to see. Architectural progress is easier to feel after the system becomes calmer.

---

## 2026-03-16 — Wanting One More Guard Is Often a Signal

When I feel like adding one more guard before writing a method, it often means the boundary below that method is not yet trusted.

Sometimes the right fix is the guard. But often the better question is: what invariant is still missing underneath?

That makes the feeling useful. The urge to add a guard is not only caution. It can be a sign that the design still needs a clearer boundary.

---

## 2026-03-19 — Coordination Belongs Where Both Ends Are Visible

### The problem

A client proposes a write through Raft. Raft commits it asynchronously. The handler needs to block until the entry is applied. Something must bridge the two — a waiter registry keyed by request ID.

The question: where does the waiter map live?

### The wrong path

My first instinct was to put the waiter inside `RaftHost`. It owns the run loop, it sees `Ready.Entries` and `Ready.CommittedEntries` — it feels like the natural place. I tried:

1. **Pass-in channel** — the handler passes `chan error` into `Propose`. But `RaftHost`'s run loop would depend on an externally created channel. The caller controls the buffer size, the lifecycle, the correctness. The run loop has to trust what it was given.

2. **Queue bridge** — `Propose` pushes the channel into a FIFO queue, the run loop pops and pairs it with the index from `Ready.Entries`. Breaks under concurrency: two concurrent proposals may enter the queue in a different order than Raft assigns indices.

3. **Return channel** — `Propose` returns `<-chan error`, created by the run loop. Requires routing proposals through the run loop to serialize assignment. Works but couples propose submission to the run loop's processing cadence.

4. **Log index as key** — use the Raft-assigned index to key the waiter map. But the handler can't register before proposing because it doesn't know the index yet. The index is only visible in `Ready.Entries`, which arrives later in a different goroutine.

Each attempt hit a wall. The common root cause: `RaftHost` knows delivery (which entries committed) but doesn't know identity (which caller proposed which entry). Pushing identity downward forces the transport layer to understand application semantics.

### What etcd does

etcd doesn't put the waiter map in `raftNode` at all. The server generates a request ID, embeds it in the proposal's data payload, registers a waiter *before* proposing, and fires `Propose` as a fire-and-forget call. The apply loop — which lives in the *server* layer, not the raft layer — deserializes committed entries, extracts the ID, and signals the matching waiter.

`raftNode` is pure plumbing. It moves `CommittedEntries` from `Ready` to an apply channel. It never touches the waiter map.

### The principle

**Coordination belongs at the layer that can see both the request identity and the apply outcome.** The server layer creates the ID and the channel (request side). The server's `Applier` implementation receives committed entries and extracts the ID (apply side). Both ends are visible at the same level. No lower layer is violated.

### What I missed initially

The request ID isn't only for idempotency — that's a bonus. Its primary job is to carry identity across the Raft boundary so the apply path can match a committed entry back to the waiting caller. The log index can't do this because it's invisible at propose time. The ID solves the coordination problem first; deduplication comes free later.

### The Applier leak

While working out coordination ownership, I noticed a second smell: `Applier.Apply([]raft.Entry)` forces the server layer to import `raft.Entry` — a Raft-internal type leaking upward through the interface. The server shouldn't need to know what a Raft entry looks like; it only cares about committed data bytes.

The fix: remove the injected `Applier` interface entirely. RaftHost pushes committed data as `[][]byte` onto a `toApply` channel. The server pulls from the channel and deserializes using its own envelope format. No Raft type crosses the boundary.

This is the same principle again — the boundary between layers should carry only what the upper layer actually needs. `[]raft.Entry` carried index, term, type, and data; the server only needed data. The interface was shaped around what the *producer* had, not what the *consumer* needed.

### What triggered the discovery

Defending discomfort each time a proposed shape felt wrong. The waiter map placement was uncomfortable because identity was invisible below. The `Applier` interface was uncomfortable because the import felt wrong. Both discomforts pointed at the same root: a layer boundary carrying more than it should. The feeling is a useful architectural signal — not perfectionism, but a smell worth investigating.

---

## 2026-03-23 — When Indirection Is Free

### The problem

Designing the raft transport, two interfaces appeared: `Raft` (so the transport can deliver messages without importing the server) and `RaftTransporter` (so the server can send without importing the transport). Both sit on module boundaries. But I hesitated — interfaces add indirection, and indirection has a cost. When is that cost acceptable?

### What I found

An indirect call through an interface has two concrete costs: no inlining (the compiler cannot paste the function body through the interface) and pointer chasing (two memory loads — itable pointer to function pointer, then jump). Together these are single-digit nanoseconds.

The question is not "does indirection cost something?" — it always does. The question is "what does the work behind the interface cost?" If the work is a network round-trip (microseconds to milliseconds), the nanoseconds of indirection vanish — rounding error on rounding error. If the work is a comparison function called inside a tight sort loop, both the comparison and the indirection are nanoseconds — same ballpark. Now the indirection is a significant fraction of the total.

Two regimes:

1. **Module boundary** — the work behind the interface is heavy (I/O, serialization, consensus step). Indirection cost is invisible. Interfaces are free here, and the decoupling they buy is pure upside.
2. **Hot inner loop** — the work behind the call is tiny (arithmetic, comparison, field access). Indirection cost is in the same order of magnitude as the work. Inline the logic or use a concrete type.

### The principle

Use an interface when the cost of crossing it is orders of magnitude smaller than the work it wraps. On a hot path where both are nanoseconds, prefer concrete types and let the compiler inline.

### What triggered this

Drawing the `Raft.Process` interface for the transport layer. A message crosses a network — microseconds minimum. The indirect call to deliver it is nanoseconds. The interface is free. Contrast with a `Less` function plugged into a sort: the comparison is nanoseconds, and the indirect call doubles the cost. Same mechanism, different regime, opposite decision.

### Interview angle

I used to ask candidates "how do you choose interface vs. concrete type?" and got back surface-level answers — runtime dispatch, no inlining, harder to navigate. True but cosmetic. The better question is "what do you trade away by introducing one?" That forces magnitude reasoning: the answer is "it depends on what's on the other side of the call." A person who can articulate the two regimes — boundary vs. hot path — has a cost model, not a feature list. The same filter generalises: "when would you *not* use a channel?", "when is copying cheaper than sharing a pointer?" Any question where the honest answer is "it depends on the ratio" separates trade-off thinkers from best-practice reciters.
