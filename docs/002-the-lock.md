## Sabotage
What happens when tens of thousands of Go routines read/write on the same `DB`?

1. The subtle cost of maintaining lock acquisitions becomes a problem.
2. Race condition becomes heavy, as the number of concurrency Go routines increase, most of the time spends on waiting.

## What to do
1. The first idea I come up with is to reduce the race condition by creating multiple buckets, 
   which comes from the design of `ConcurrentDictionary<TKey, TValue>` of C# (the same as its counterpart of Java), hence multiple `RWMutex`s for each bucket respectively.
2. I also need a tool to help measure the improvement after applying the idea above.