# Sabotage
What happens if the primary dies?

## The Experiment
We kill the primary node while the system is under load.

```powershell
.\scripts\crash-test.ps1 -KillPrimary
```

## The Reality
When the primary process vanishes:
1.  **Writes Halt:** Clients receive "Connection Refused."
2.  **Replicas Drift:** They log "replication stream closed" and stop receiving updates.
3.  **The Lock-in:** The replica *knows* the connection is dead, but it remains in "Replica Mode." It continues to reject writes (`OpPut`) because its configuration says `isReplica=true`.

## The Gap
The system has a valid copy of the data (on the replica), but we cannot use it.
To fix this today, an operator must:
1.  SSH into the replica.
2.  Kill the process.
3.  Restart it without the `--replica-of` flag.

This "Restart-to-Fix" pattern is unacceptable for high availability. We lose the in-memory cache and incur downtime during the restart.

## What to do
We need **Runtime Reconfiguration**.

We don't need full consensus (Raft) yet. We just need a way to tell a running replica: "You are the captain now."

**The Plan:**
1.  **Protocol**: Add `OpPromote`.
2.  **Mechanism**:
    - When a replica receives partial `OpPromote`:
    - It clears its `isReplica` flag.
    - It stops the background replication loop.
    - It unlocks the write path.
    - It logs "Role changed to Primary."

This transforms the fix from "Restart Process" to "Send Command." It paves the way for a future automated Sentinel.
