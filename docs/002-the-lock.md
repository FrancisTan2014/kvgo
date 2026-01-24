# Sabotage
What happens when tens of thousands of goroutines read and write to the same `DB`?

1. The subtle cost of maintaining lock acquisitions becomes a problem.
2. Contention becomes severe: as concurrency increases, more time is spent waiting.

## What to do
1. My first idea is to reduce contention by sharding the map into multiple buckets.
   This is inspired by C#’s `ConcurrentDictionary<TKey, TValue>` (and similar designs in Java): multiple buckets, each protected by its own `RWMutex`.
2. I also need a way to measure the improvement after applying this change.