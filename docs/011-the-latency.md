# Sabotage
What if nodes run on different machines?

## What we measured

```powershell
.\scripts\bench.ps1 -Requests 100000 -Concurrency 100 -Replicas 10 -GetRatio 0.5
```

```
Throughput:     46,377 req/s
P50:            1.37ms
P99:            10.44ms
```

vs Redis ([source](https://redis.io/docs/latest/operate/oss_and_stack/management/optimization/benchmarks/)):

| System | Throughput | P50 |
|--------|------------|-----|
| kv-go | 46k req/s | 1.37ms |
| Redis | 180k req/s | 0.14ms |

4x slower, 10x worse latency. Expected — Redis has years of optimizations we bypassed.

But both ran on localhost.

## What we missed

```
Localhost:      ~0.05ms
Same DC:        ~0.5ms
Cross-AZ:       ~1-3ms
Cross-region:   ~70-150ms
```

Add network to P50:

| Environment | kv-go | Redis | Gap |
|-------------|-------|-------|-----|
| Localhost | 1.37ms | 0.14ms | 1.23ms |
| Cross-region (+70ms) | 71.37ms | 70.14ms | 1.23ms |

**Gap stays constant. At 70ms network cost, 1.23ms code difference is noise.**

## What matters

Local benchmarks measure code, not systems.

Network is the great equalizer — micro-optimizations disappear into physics.

## What to do

Inject 70ms latency to replication. See what actually breaks.

## References

- https://redis.io/docs/latest/operate/oss_and_stack/management/optimization/benchmarks/