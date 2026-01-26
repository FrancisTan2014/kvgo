# kv-go
This repo is my starting point for building things from scratch to understand what’s happening “under the hood”.

By building a key-value store, I want to:
1. Apply what I’ve learned from CS:APP
2. Learn Go by writing real code
3. Learn how modern distributed systems work

## Sabotage
I think of this as a question-driven methodology: I start with questions and let them guide me through distributed
systems. This helps me avoid getting lost in details and build a solid mental model faster.

## Skipped
1. Currently uses an in-memory model. Future roadmap includes moving values to disk (Bitcask architecture) to reduce startup time from O(DataSize) to O(KeyCount).