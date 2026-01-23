# The start
To build a key-value storage module, keys and values are both strings.
`Map` is the natual structure that comes up with my mind, and I'm gonna write a tough one first.
As far, we have two components only, the `Map` and the `Store`.
At this moment, they have no differences, but they will serve different roles in the future.
`Put` and `Get` are the only APIs currently.

Before writing the code, I need to learn how to write a module in the Go language.
1. Go code is grouped into packages, and packages are grouped into modules. 
2. Folder as package, prefer flat folder hierarchy.

Aha, `map` is a built-in feature of `Go`, I don't need to write a `Map` from the scratch at the moment.