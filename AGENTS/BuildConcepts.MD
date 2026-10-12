# Build and architecture

This is the short, current engineering guide. See [README.md](README.md) for
the documentation map and `AGENTS.md` at the repository root for agent workflow.

## Packages

- `common`: schema, entity/namespace kinds, identifiers, durability.
- `index`: schema-defined unified store, liveness, recovery, typed accessors;
  `index/{bitmapidx,timeidx,geoidx,radixhash,simhash,vectoridx}` implement the
  six engine families. Index packages do not import `search`.
- `roar`, `internal/storage`, `internal/pagealloc`: bitmap and storage mechanisms.
- `search`: immutable conjunctive plans, entity-scoped queries via `Engine`,
  pooled `Executor`, metrics, optional `ranking` package.
- `cmd/imageidx`: runnable unified-store HTTP/one-shot demos;
  `examples/basic`, `examples/service`, and `examples/bitmaponly` show entity-
  scoped queries, reusable plans, and BSI ranges.

## Check changes

```sh
go build ./...
go test ./...
go test -race ./...
sigmap validate
```

Run focused tests first and benchmark modified hot paths with
`go test ./search ./index/bitmapidx -run '^$' -bench '<name>' -benchmem`.
Queries see independently published index views, **not** an atomic snapshot
across engines. `index.Store` batches use liveness and recovery markers to
guard partial writes; see `index/README.md` for the full contract.
