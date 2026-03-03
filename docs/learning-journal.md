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
