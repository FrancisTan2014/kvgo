# The Start
To build a key-value storage module, keys and values are both strings.
`map` is the natural structure that comes to mind, so I'm going to write a rough version first.

So far, we only have two components: `Map` and `Store`.
Right now, they don't differ, but they will serve different roles in the future.
`Put` and `Get` are the only APIs for now.

Before writing code, I need to learn how to structure a module in Go:
1. Go code is grouped into packages, and packages are grouped into modules.
2. A folder is a package; prefer a flat folder hierarchy.

Aha—`map` is built into Go, so I don't need to implement a `Map` from scratch yet.