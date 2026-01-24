# Sabotage
`file.Sync()` becomes the bottleneck. What policy should I choose to balance durability and throughput?

## Analysis
This reminds me of isolation levels in relational databases: instead of one “correct” setting, systems usually offer a small set of trade-offs.

Once `file.Sync()` dominates the write path, there are two extremes:
`Flush immediately <-----------------------> Let the kernel deal with it`

- Flush immediately: strongest durability, worst latency/throughput.
- Let the kernel deal with it: best performance, but recent writes may be lost on crash or power loss.

Most designs live between these points. I can model it as a few durability levels, or pick a single practical default.
One reasonable middle ground is group commit: flush the buffer periodically (or after N writes).

## What to do
After reading up on common approaches, I’m leaning toward `Strict Group Commit`: no acknowledged writes are lost, while throughput stays reasonable.
It’s also a good exercise for mastering Go’s concurrency toolchain.
Below is a diagram of how `Strict Group Commit` would work in `kvgo`:

```mermaid
sequenceDiagram
    autonumber
    actor C1 as Client 1
    actor C2 as Client 2
    participant Put as DB.Put() (Goroutine)
    participant Ch as writeCh (Buffered)
    participant W as Worker Loop
    participant Disk as OS / Disk

    Note over C1, C2: High Concurrency Scenario

    %% Client 1 Logic
    C1->>Put: Put("A", "1")
    Put->>Put: Create Request {Data, respCh}
    Put->>Ch: Send Request
    activate Put
    Note right of Put: BLOCKED (Waiting on respCh)

    %% Client 2 Logic
    C2->>Put: Put("B", "2")
    Put->>Put: Create Request {Data, respCh}
    Put->>Ch: Send Request
    activate Put
    Note right of Put: BLOCKED (Waiting on respCh)

    %% Worker Logic
    Note over W: 1. Accumulation Phase
    W->>Ch: Read Req 1
    W->>W: Add to Batch Slice
    W->>Ch: Read Req 2
    W->>W: Add to Batch Slice

    Note over W: 2. Commit Phase (Timer or MaxSize)
    W->>Disk: file.Write(Batch)
    W->>Disk: file.Sync() (The Expensive Call)
    disk-->>W: OK

    Note over W: 3. Notification Phase
    W->>W: Update Memory Map
    W-->>Put: Send "OK" to respCh 1
    deactivate Put
    W-->>Put: Send "OK" to respCh 2
    deactivate Put

    Put-->>C1: Return nil
    Put-->>C2: Return nil

```

```mermaid

flowchart TD
    Start((Start Worker)) --> Init[Init Batch Slice]
    Init --> Select{Select Case}

    %% Case 1: New Data Arrives
    Select -- case req := <-writeCh --> Append[Append req to Batch]
    Append --> CheckLimit{Batch Size >= Max?}
    
    CheckLimit -- No --> Select
    CheckLimit -- Yes --> CommitStep

    %% Case 2: Timer Ticks (Latency Guarantee)
    Select -- case <-timer.C --> CommitStep[Commit Phase]

    %% The Commit Logic
    subgraph Critical Path [The Commit Phase]
        CommitStep --> Encode[Encode Binary Data]
        Encode --> IO[Write + Fsync]
        IO --> Map[Update DB.data Map]
        Map --> Notify[Loop Batch: Send to respCh]
    end

    Notify --> Clear[Clear Batch Slice]
    Clear --> Select

```

## Graceful shutdown
`Strict Group Commit` is only useful if shutdown doesn’t strand callers.
The shutdown goal is: stop accepting new writes, drain any queued writes, flush once, then release resources.

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant Put as DB.Put()
    participant Ch as reqCh (Buffered)
    participant W as Worker Loop
    participant Disk as OS / Disk
    participant Close as DB.Close()

    Note over Put,W: Normal write path
    C->>Put: Put("K", "V")
    Put->>Ch: enqueue writeRequest
    activate Put

    W->>Ch: dequeue writeRequest
    W->>Disk: Write(batch)
    W->>Disk: Sync()
    Disk-->>W: OK
    W-->>Put: respCh <- nil
    deactivate Put
    Put-->>C: return nil

    Note over Close,W: Shutdown begins
    C->>Close: Close()
    Close->>Close: mark closed + close(closedCh)
    Close->>W: close(stopCh)

    Note over W: Drain queued requests (non-blocking)
    loop drain until empty
        W->>Ch: try dequeue
        W->>W: append to batch
    end

    Note over W: Final flush (if batch not empty)
    W->>Disk: Write(batch)
    W->>Disk: Sync()
    Disk-->>W: OK
    W-->>Put: respCh <- err/nil (for each drained request)

    W-->>Close: close(doneCh)
    Close->>Disk: Close WAL file
    Close-->>C: return

    Note over Put: After Close() starts
    C->>Put: Put("K2", "V2")
    Put-->>C: ErrClosed

```

```mermaid
flowchart TD
    A["Close"] --> B["Set closed flag"]
    B --> C["Close closedCh (broadcast shutdown)"]
    C --> D["Close stopCh (stop worker)"]
    D --> E["Worker drains reqCh"]
    E --> F{Any buffered requests?}
    F -- Yes --> G["Encode batch"]
    G --> G2["Write + Sync"]
    G2 --> H["Reply to each respCh"]
    F -- No --> I["Skip flush"]
    H --> J["Close doneCh"]
    I --> J
    J --> K["Stop ticker"]
    K --> L["Close WAL"]
    L --> M["Return"]
```