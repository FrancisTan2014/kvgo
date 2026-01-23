# Sabotage
What if the process crashes or power is off at any time?

We lost the data, as a database, it is not acceptable.

## What to do
`Write-Ahead Log` is the way that many other databases take to persist the data.
The basic idea is to have a log file open once and append logs to it without seeking on the disk.

A single protocol would be:
`LenK (4bytes) + LenV (4bytes) + Key + Value`

Thus, we can use a state machine to replay the database on restarting it.