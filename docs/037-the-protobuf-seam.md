# 037 — The Protobuf Seam

## The problem

The next step is a Raft transport — TCP connections that carry `raft.Message` between nodes. The blocker: `Message` has no wire format. `Entry`, `HardState`, and `SnapshotMeta` already have hand-rolled binary codecs, but those are flat, fixed-size structs. `Message` is different: it nests a variable-length slice of `Entry` and an optional `SnapshotMeta`.

Hand-rolling another codec is doable, but the learning value is gone. The project already has three hand-written codecs. Writing a fourth for a nested, variable-length struct teaches nothing new — it's the same pattern with more fields. Protobuf exists to solve exactly this kind of encoding. Using it is the new lesson.

## The `raftpb` package

A `raftpb` package owns the `.proto` file and the generated Go code. The `.proto` defines `Entry`, `HardState`, `SnapshotMeta`, and `Message`. This is the single source of truth for the Raft type definitions.

`src/raft/` and `src/server/` import `raftpb` and use the generated types directly with the `raftpb.` prefix — no aliases, no wrapper structs. Helper functions that aren't part of the schema (`IsEmptyHardState`, `hardStatesEqual`) live in the files that call them (`raft.go` and `node.go`), not in `raftpb`.

## The WAL stays hand-rolled

The WAL codec in `message.go` stays as-is. It reads and writes fields from the protobuf-generated structs, but uses its own binary format — not `proto.Marshal`.

Why not use `proto.Marshal` for the WAL too? The WAL batches entries, hard state, and snapshot metadata into a single `SaveBatch` record. `SaveBatch` is a storage-internal batching concept. Putting it in the `.proto` alongside wire types like `Message` conflates two concerns: what goes on the network vs how the WAL organizes its writes.

## Pointer semantics

Modern `protoc-gen-go` (backed by `google.golang.org/protobuf`) embeds `protoimpl.MessageState` in every generated struct. `MessageState` contains a `sync.Mutex`. That makes the generated types non-copyable — passing them by value triggers `go vet` copylocks warnings.

Every proto type is a pointer throughout: `*raftpb.Entry`, `*raftpb.HardState`, `*raftpb.Message`. This is the opposite of etcd's `raftpb`, which uses the deprecated `gogo/protobuf` that generates plain structs without internal mutexes.

**Consequence**: returning `[]*raftpb.Entry` from a function like `entriesBetween()` means the caller holds pointers into the leader's log. A deep copy at the export boundary prevents external mutation of internal state.

## Go protobuf toolchain

In C#, adding a `.proto` to the project and setting its build action to `Protobuf` in the `.csproj` is enough — MSBuild runs code generation automatically on every build. The generated classes live in `obj/` and are never checked in.

Go has no integrated build system that does this. The workflow:

1. Install `protoc` (the compiler) and `protoc-gen-go` (the Go plugin) as separate binaries.
2. Run the command manually:
   ```
   protoc --go_out=. --go_opt=paths=source_relative raft.proto
   ```
3. The output (`raft.pb.go`) is a regular Go source file. **It is checked into version control.** `go build` compiles it like any other `.go` file — it has no idea protobuf was involved.

There is no build hook or NuGet equivalent that runs codegen transparently. If the `.proto` changes, re-run `protoc` and commit the updated `.pb.go`. This is by design — Go's build philosophy is "everything needed to compile is in the source tree."

## Open threads

1. **Wire codec for `raft.Message`**: `proto.Marshal` / `proto.Unmarshal` with length-prefixed framing over the transport's TCP connections.

2. **`proto.Marshal` side-effects**: marshaling sets an internal `sizeCache` field on the struct. Code that compares proto values with `reflect.DeepEqual` (including `require.Equal` in tests) can break if one copy was marshaled and the other wasn't. Use field-by-field comparison or `proto.Equal` instead.
