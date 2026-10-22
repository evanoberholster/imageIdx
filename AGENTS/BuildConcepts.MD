# Build and architecture

This is the short, current engineering guide. See [README.md](README.md) for the
documentation map and [`AGENTS.md`](../AGENTS.md) for agent workflow.

## Package direction

- `schema`: namespace definitions, entity identity, identifier aliases,
  durability, and durable ValueIDs. It is the schema authority.
- `database`: primary V1 lifecycle; owns `schema.dat`, canonical Pebble records,
  the ordered journal, and one derived `index.Store`. It coordinates commits,
  indexing waits, recovery, and checkpointing.
- `index`: advanced unified index store and typed routing; owns bitmap, time,
  geography, exact-hash, perceptual-hash, and vector engines.
- `index/{bitmapidx,timeidx,geoidx,radixhash,simhash}`: advanced stable engine
  families. `index/vectoridx` is experimental in V1.
- `store`: advanced Pebble-backed KV primitive used for canonical records and
  the journal. It does not import `database` or `index`.
- `roar`, `internal/storage`, `internal/pagealloc`: bitmap and storage
  mechanisms. `internal/*` is private.
- `search`: immutable conjunctive plans, entity-scoped `Engine`, pooled
  `Executor`, metrics, and optional ranking.
- `cmd/imageidx`: CLI/HTTP demos and examples. The `database` example is the V1
  primary workflow; the other examples show advanced standalone `index.Store`.

Index packages must not import `search`. `database` does not import `search`;
applications bind it with `search.New(db.Index())`.

## Consistency

- Canonical `DB.Commit` writes authoritative records and the journal durably
  before returning.
- Derived indexing is asynchronous; wait on the returned generation with
  `DB.WaitIndexed` before asserting search freshness.
- `search.Engine` plans pin one coherent snapshot per execution. Direct
  `search.Compile` plans read live engine views.
- `index.Store` itself is not an atomic multi-engine write transaction. It uses
  prevalidation, liveness ordering, and recovery markers to prevent incomplete
  entities from becoming searchable.
- Writers are serialized. Do not add per-family parallel publication without a
  composite root and safe reclamation proof.

## Check changes

```sh
go fmt ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
sigmap validate
```

Run focused tests first and benchmark modified hot paths with
`go test ./search ./index/bitmapidx -run '^$' -bench '<name>' -benchmem`.
Do not claim a command passed unless it was actually run.
