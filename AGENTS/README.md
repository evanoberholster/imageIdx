# imageIdx engineering documentation

This directory is the current architecture and contributor guide for imageIdx.
It replaces the previous collection of per-subsystem proposals and phase reports.
Historical material remains in git history; it is not a current API contract.

Read these three files:

| Document | Purpose |
|---|---|
| [README.md](README.md) | Orientation, package map, workflow, and authority |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Current runtime structure, data flow, concurrency, and recovery |
| [CONTRACTS.md](CONTRACTS.md) | Stable/advanced/experimental APIs, ownership, limits, and release policy |

For public usage and persistence details, the package documentation is
authoritative:

- [`README.md`](../README.md) — V1 user guide and quick start
- [`database/README.md`](../database/README.md) — primary application lifecycle
- [`database/FORMAT.md`](../database/FORMAT.md) — canonical, journal, key, and recovery formats
- [`schema/README.md`](../schema/README.md) and [`schema/FORMAT.md`](../schema/FORMAT.md) — schema and IDs
- [`index/README.md`](../index/README.md) — unified/standalone index store
- [`search/README.md`](../search/README.md) — plans, workspaces, and snapshots
- [`store/README.md`](../store/README.md) — advanced KV primitive

## Package map

| Package | Role | V1 status |
|---|---|---|
| `schema` | Namespace definitions, entity identity, `schema.dat`, durable ValueIDs | Stable |
| `database` | Primary lifecycle, canonical records, journal, indexing coordinator, recovery | Stable |
| `store` | Pebble-backed KV primitive for canonical records and the journal | Advanced stable |
| `index` | Unified index store, namespace routing, liveness, recovery markers | Advanced stable |
| `index/bitmapidx` | Enum, bool, text, continuous, and BSI bitmap namespaces | Advanced stable |
| `index/timeidx` | Exact timestamp index | Advanced stable |
| `index/geoidx` | S2 geographic index | Advanced stable |
| `index/radixhash` | Exact 256-bit hash index | Advanced stable |
| `index/simhash` | pHash/PDQ perceptual similarity, dense and sparse IDs | Advanced stable |
| `index/vectoridx` | HNSW vector index | Experimental |
| `search` | Reusable conjunctive plans, snapshots, executors, metrics | Stable |
| `search/ranking` | Optional ranking/fusion helpers | Advanced |
| `roar` | Immutable roaring bitmaps and kernels | Advanced stable |
| `internal/*` | Private storage, kernels, benchmark harness | Internal |
| `cmd/imageidx` | CLI, HTTP demos, and runnable examples | Examples/tools |

`database.DB` is the stable application entry point. `index` and `store` remain
public for advanced and standalone use. Vector APIs, performance, and file
formats are explicitly experimental in V1.

## Data flow

```text
application Mutation
        |
        v
database.Commit
  canonical value + ordered journal + generation (one synced Pebble batch)
        |
        v
database coordinator
  decode projection -> diff/coalesce -> index.Batch
        |
        v
index.Store
  serialized writer -> bitmap/time/geo/hash/simhash/vector engines
  -> unified manifest + source generation
        |
        v
index durable and visible
        |
        v
Pebble acknowledgment -> journal cleanup
```

Searches never participate in the write path. `WaitIndexed(ctx, generation)`
gates freshness when an application needs read-after-index behavior. `search.Engine`
plans pin one coherent snapshot for the duration of an execution; writers do not
hold a query-wide lock.

## Contributor workflow

1. Locate the smallest relevant surface with SigMap.
2. Read the current implementation and tests.
3. Make the smallest correct change.
4. Run focused tests.
5. Run the broad validation commands below before handoff.

```sh
go fmt ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
sigmap validate
```

Use `IMAGEIDX_BENCH_LARGE` only for opt-in large benchmarks. Do not replace the
shared `internal/bench` harness with a parallel framework.

## Documentation rules

- Current source, tests, and package docs take precedence over this directory.
- Do not add a second implementation proposal under `AGENTS/`. Update the
  current architecture or contracts instead.
- Historical phase instructions and design reports are available in git history.
- Package READMEs should describe public behavior. This directory explains how
  the pieces fit together.
