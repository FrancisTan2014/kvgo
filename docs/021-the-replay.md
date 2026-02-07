# Sabotage

It takes 46 seconds to restart a primary node with a 1.4 GB WAL file.

## Sabotage Scenario

```
1. Start primary on port 5000
2. Write 10M keys (100 concurrent connections, 128-byte values)
   - Completes in ~166 seconds
   - Creates 1.4 GB WAL file
   - Consumes 2.5 GB RAM
3. Stop the primary
4. Restart the primary
5. Observe: startup takes 46 seconds
   - Replays entire 1.4 GB WAL
   - Re-executes 10M Put() operations
   - Rebuilds in-memory hash tables
6. During this window: database is unavailable
```

**The constraint:** Startup time is O(DataSize), not O(KeyCount).

## Thinking
Obviously, 46 seconds to restart a primary node on PROD is unacceptable.
During that window, followers might elect a new leader. Wait, no—that's out of scope here.
I've heard about leader election somewhere, but I don't really understand it yet.

It's painful thinking about problems I've never faced before. 
Don't give up.
As a system-level engineer, I can't always wait for others to give me tasks. I need to generate requirements by analyzing the system myself.

Our system is hitting an I/O bottleneck. We need to change the architecture of how we write and read WAL logs. 
The question is: how do we read or write less so replay completes quickly?

Can we write less? I can't imagine how without causing data corruption. Compression? That doesn't solve it—compress and decompress still take compute time.

Can we read less? 
Who's reading the data during replay? The DB engine, not the end user. 
Following this logic, **values are useless during replay**. 
A lazy loading mechanism surfaces naturally: read all keys into memory during replay (fast), then when a `Get` request arrives, seek the value from disk. This makes sense.

Diving deeper: how do we get the expected value without corruption? Recall how we designed the protocol—we need the frame size (the value bytes) and we need to tell the kernel where to start reading. This design introduces additional latency on `Get` requests from nanoseconds to milliseconds. From CS:APP, I know disk seeks take 10-50ms. Wait—we can hold the file descriptor open and reuse it, just modifying the offset via system call. The disk seek cost only happens once when opening the file.

Let me try to write the design:
1. We now have two files for the WAL: one for keys, one for values.
2. Each write request produces two log entries:
    - Key: `[keyLen : 4 bytes][valueLen : 4 bytes][offset : 4 bytes][key bytes]`
    - Value: `[value bytes]`

But how do we compact the log now? After checking the compaction logic, I think I don't need to change it. 
Let it be as it is: stop the world, compact, and write. Maybe there's a well-known solution—I don't know, it's beyond what I can imagine right now.

---
## Coach feedback

What You Should Do Next
Before implementing, answer these:

Replay logic: On startup, you read the key file. Each entry has an offset. Do you validate that the value exists at that offset? Or trust it blindly?

Failure atomicity: Currently, a Put is atomic (one write to WAL). With two files, you write value first, then key. 
What if crash happens between? (Hint: you'll have orphaned values. Is that okay?)

File management: Do you keep one value file forever? Or rotate it? If you rotate, how do offsets work across multiple files?

---

## Keep thinking

The coach pointed out that I'm building a subsystem here. The entry point is the pain we hit when replaying a large WAL log. 
Actually, "naively designed" isn't quite right—the single-WAL design isn't worthless, it's the foundation that got us here. It works, just doesn't scale.

All three questions come down to one thing: **how do we ensure data integrity?** 

The first question seemed like it was about defending against external corruption (other processes messing with our files). 
We already handled that with file locks to prevent duplicate `kv-server` processes on the same machine. 
But that's not what this is about. This is about corruption from **sudden crashes**. 
I think this kind of corruption always happens at the tail of the file—we'll hit `EOF` when replaying corrupted entries. 
How many key-value pairs get corrupted? No idea. The data is lost. So the answer is: drop keys with invalid offsets during replay.

Failure atomicity. This reminds me of database transactions, but I've never implemented one before. 
With two writes, you're supposed to rollback both if either fails. But what if the rollback itself fails? 
I don't know. I gave up on this line of thinking—it's a recursive trap.

The third question makes sense. We should maintain small value log files, like how we shard data in memory. 
This also gives us room to improve the `Get` latency we're introducing. 
We could start a background worker to load popular values into memory. 
We'd need to add a file ID to the key entry to track which value file it points to.

---

## Coach feedback

There is no rollback in append-only logs.

Write order:

Append value to value file
fsync value file (durable)
Append key+offset to key file
fsync key file (durable)
If crash at step 1-2 → partial value at tail, no key pointing to it → safe
If crash at step 3-4 → partial key at tail, discard on replay → safe

No rollback needed. The key is the "commit point." If the key is durable, the write is committed. If not, the value is invisible.

Left a question: how to rotate the value files?

---

## Keep thinking

What if we just rotate when a file grows to a certain size?
No, that increases compaction complexity.
What if we reuse the sharding model from memory?
That makes sense. The architecture becomes simpler—we don't have complicated state to maintain. 

---

## Coach feedback

But You're Still Avoiding The Question
When shard 0 compacts:

**Before:**

keys.0 (old, has dead keys)
values.0 (old, has dead values)

**After compaction, you write:**

keys.0.new (only live keys)
values.0.new = what?
You have 3 choices:

**A. Copy all live values to new value file**

Pro: Clean, self-contained
Con: Doubles disk I/O (read old values, write new values)

**B. Keep old value file, just rewrite keys**

Pro: No value copying
Con: Keys point to old value file—need to track multiple value files per shard

**C. Don't compact value files at all, only compact keys**

Pro: Simplest
Con: Value files grow forever, wasted space never reclaimed

---

## Keep thinking
I choose C.
But we can't ignore the file growth problem.
We'll provide the operator a cleanup function for value files.

The coach said the design is done.

---

## Design

### WAL file structure
`index file + value file` as a unit for each shard, e.g.: for shard 0 we got index0 and value0.

### Write path
In this architecture, a single lock and group committer for the whole `DB` isn't enough.
We now maintain a `group committer + compaction worker` pair for each shard. 
For each shard, we maintain an `Offset` and update it atomically when each write happens.
Since we write to two files, atomicity must be guaranteed. 
When the group committer flushes memory:
1. Write values. If `fsync` fails, the machine is untrustworthy—terminate the process.
2. Write indexes. If `fsync` fails, terminate.
3. Return success to all clients in the batch.

### Read path
Now the value of the map entry in each shard becomes a struct:
```Go
type ValueEntry struct {
    Payload []byte
    Size    int
    Offset  int
}
```

When a client acquires this value:
```Go
if Payload != nil {
    return Payload
} else {
    return seekFromDisk()
}
```

When do we populate the Payload?
- First Get: seek disk, store in Payload
- Second Get: return cached Payload
- Need to track memory usage

### Replay
For each shard:
  Open index.N and value.N
  For each entry in index.N:
    Read [keyLen, valueLen, offset, key]
    If EOF or short read → stop (tail corruption)
    If offset >= value file size → skip (invalid)
    Store in shard map: key → ValueEntry{Payload: nil, Size: valueLen, Offset: offset}

### Compaction
Not much change, works as before.

### Cleanup
This is a new command for `kv-go`. The operator determines when to clean value files.
We return errors to clients during cleanup—document this as a "maintenance window."
For each shard:
1. Stop accepting writes
2. Read all live keys from memory
3. For each key: read value from old `values.N`, write to `values.N.new`
4. Update offsets in memory to point to new file
5. Write new index file with updated offsets
6. Atomically swap files: `values.N.new` → `values.N`, `index.N.new` → `index.N`
7. Resume writes

### Engine adjustment
Refactor `DB` and `shard`. Let each `shard` hold its own WAL files and background workers.