# Sabotage
What happens to the *other* replicas?

## The Experiment
We need a bigger cluster to see this failure.
Topology: **1 Primary, 2 Replicas**.

1.  Start Primary (P).
2.  Start Replica 1 (R1) -> follows P.
3.  Start Replica 2 (R2) -> follows P.
4.  **Kill P.**
5.  Promote R1 (using `OpPromote` from 013).

## The Reality
-   **R1** is now a Primary. It is healthy and accepting writes.
-   **R2** is an orphan. It is still strictly loyal to the dead P.
    -   It keeps trying to dial P.
    -   It logs connection failures.
    -   It has no idea R1 exists or that R1 is the new leader.

## The Gap
We fixed the "Headless" problem in 013, but we created a "Split Fleet" problem.
R1 has moved on, but R2 is left behind. To fix R2, we currently have to restart it with a new `--replica-of` flag.
Again, "Restart-to-Fix" is the enemy.

## What to do
We need **Runtime Repointing**.

We need a way to tell a running replica: "Stop following X, start following Y."

**The Plan:**
1.  **Protocol**: Add `OpReplicate` (re-use existing Op? No, `OpReplicate` (3) is the handshake *request* sent by a replica. We need a *command* sent by admin).
    -   Let's call it `OpRelocate` or `OpFollow`.
    -   Actually, let's stick to Redis terminology: `REPLICAOF <host> <port>`.
    -   Protocol: `OpReplicaOf` (6). Payload: `[host string]`.
2.  **Mechanism**:
    -   Stop existing replication loop (if any).
    -   Connect to the new target.
    -   Perform handshake.
    -   Start receiving writes.
