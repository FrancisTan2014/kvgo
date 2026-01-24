# Sabotage
What if the process crashes, or the machine loses power at any time?

If we lose data, that’s not acceptable for a database.

## What to do
`Write-Ahead Log` (WAL) is a common approach databases use to persist writes.
The basic idea is to open a log file once, then append records to it (no seeking).

One simple record format is:
`KeyLen (4 bytes) + ValueLen (4 bytes) + Key + Value`

With that, we can replay the log on startup to rebuild the in-memory state.