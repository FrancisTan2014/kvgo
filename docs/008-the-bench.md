# Sabotage
Can our `kv-server` handle 100k requests per second?

## Analysis
Even without a benchmark, I can confidently say no.
Many default values in our design were chosen arbitrarily.
Time to build a benchmark and tune.

## The benchmark tool

`kv-bench` opens N connections and fires requests as fast as the server allows:

```bash
kv-bench -c 50 -n 100000 -size 128 -get-ratio 0.0
```

## First run: the 100ms mystery

```
Throughput:     499 req/s
P50:            100ms
```

**500 req/s with P50 = 100ms?** Suspiciously round numbers.

### Root cause

`syncInterval = 100ms` in the group committer. Every `Put()` blocks until the next WAL flush.

Throughput formula for group commit:

$$\text{throughput} = \frac{\text{concurrency}}{\text{sync interval}} = \frac{50}{0.1s} = 500 \text{ req/s}$$

Not a bug—this is the durability guarantee working as designed.

## Tuning

Made `syncInterval` configurable via `-sync` flag:

```bash
kv-server -data-dir ./data -sync 10ms
```

Result with 10ms:

```
Throughput:     4,979 req/s
P50:            10ms
```

10x faster, exactly as predicted.

## Tradeoffs

| `-sync` | P50 | Throughput (50 conn) | Use case |
|---------|-----|----------------------|----------|
| 100ms | ~100ms | 500 req/s | Financial: durability first |
| 10ms | ~10ms | 5,000 req/s | Web apps: balanced |
| 1ms | ~1ms | 50,000 req/s | Analytics: speed first |

## What's next

To go faster without weakening durability: pipelining, client-side batching, or async replication.