# imageIdx

imageIdx is a persistent, schema-aware image indexing and search library for Go.
It stores authoritative application records in Pebble and derives bitmap,
timestamp, geographic, exact-hash, perceptual-hash, and vector indexes from
canonical projections.

This is the V1 documentation baseline. The stable entry point for applications
is [`database.DB`](database/README.md). Lower-level `index`, `store`, `schema`,
and `search` packages remain available for advanced and standalone use.

## Start here

- [Database integration](database/README.md) — canonical records, commits,
  indexing waits, search integration, recovery, and public contracts.
- [Database on-disk format](database/FORMAT.md) — V1 canonical, journal, key,
  and schema compatibility rules.
- [Schema and entity IDs](schema/README.md) — namespace definitions and durable
  `schema.dat` allocation.
- [Unified index](index/README.md) — standalone/advanced index store.
- [Search](search/README.md) — reusable plans, workspaces, and execution.
- [V1 contracts](AGENTS/CONTRACTS.md) — stable, advanced, and experimental API
  classification, limits, and release policy.

## Package map

| Package | Role | V1 status |
|---|---|---|
| `database` | Authoritative Pebble records, mutation journal, derived-index coordination, recovery | **Stable** |
| `schema` | Namespace definitions, entity identity, durable ValueID allocation | **Stable** |
| `search` | Conjunctive plans, workspaces, executors, ranking helpers ([guide](search/README.md)) | **Stable** |
| `index` | Unified index store and namespace routing ([guide](index/README.md)) | Advanced stable |
| `index/bitmapidx` | Enum, bool, text, continuous, and BSI bitmap namespaces | Advanced stable |
| `index/timeidx` | Exact timestamp index | Advanced stable |
| `index/geoidx` | S2 geographic index | Advanced stable |
| `index/radixhash` | Exact 256-bit hash index | Advanced stable |
| `index/simhash` | pHash/PDQ perceptual similarity store | Advanced stable |
| `index/vectoridx` | HNSW vector index | **Experimental** |
| `store` | Generic Pebble-backed KV collection ([guide](store/README.md)) | Advanced stable |
| `roar` | Immutable roaring-bitmap representation and operators | Advanced stable |
| `search/ranking` | Optional ranking/fusion helpers | Advanced |
| `internal/*` | Private storage, kernels, benchmarks | Internal |
| `cmd/imageidx` | CLI, HTTP demo, and examples | Examples/tools |

Vector APIs, vector file formats, vector performance, and vector compatibility
are **experimental** in V1. They remain functional and tested, but are not part
of the stable V1 compatibility promise.

## Durable and derived data

There are three distinct durability layers:

1. **`schema.dat`** is the authoritative schema, entity/namespace model,
   projection-version store, and durable entity ID allocator.
2. **Pebble canonical records and the mutation journal** are the authoritative
   application data. A successful `DB.Commit` writes both durably before it
   returns.
3. **Derived indexes** are rebuildable projections. Index files are versioned and
   fail closed when unsupported or corrupt; they are not promised to be
   byte-compatible forever. `database` rebuilds the complete index from
   canonical projections when recovery requires it.

No pre-V1 experimental database is guaranteed to open. V1 databases must reopen
with V1; unsupported container, journal, canonical, or index versions are
rejected explicitly rather than reinterpreted.

## Quick start

The current bootstrap creates the derived index directory first with
`index.Create`, then opens `database.DB`. This is a one-time creation step; later
opens use `database.Open` directly.

```go
package main

import (
    "context"
    "encoding/binary"
    "log"

    "github.com/evanoberholster/imageIdx/database"
    "github.com/evanoberholster/imageIdx/index"
    "github.com/evanoberholster/imageIdx/index/bitmapidx"
    "github.com/evanoberholster/imageIdx/schema"
    "github.com/evanoberholster/imageIdx/search"
)

const tagNamespace schema.Namespace = 101

func decodeProjection(p []byte) (index.Entry, error) {
    if len(p) != 9 {
        return index.Entry{}, database.ErrCorruptJournal
    }
    return index.Entry{
        ValueID: binary.BigEndian.Uint64(p[:8]),
        Bitmaps: []index.BitmapValue{{
            Namespace: tagNamespace,
            Value:     bitmapidx.EnumBytesValue(p[8:9]),
        }},
    }, nil
}

func main() {
    ctx := context.Background()
    defs := schema.NewDefinitions()
    tag, err := index.BitmapDef(bitmapidx.NamespaceSpec{
        Namespace: tagNamespace, Kind: schema.KindBitmapEnum,
    }, "tags", schema.EntityImage)
    if err != nil { log.Fatal(err) }
    if err := defs.Register(tag); err != nil { log.Fatal(err) }

    if _, err := index.Create("./index", index.Spec{Schema: defs}); err != nil {
        log.Fatal(err)
    }
    db, err := database.Open("./data", "./index", decodeProjection, database.Options{})
    if err != nil { log.Fatal(err) }
    defer db.Close()

    id := schema.ImageID(100)
    projection := make([]byte, 9)
    binary.BigEndian.PutUint64(projection[:8], uint64(id))
    projection[8] = 'f'
    generation, err := db.Commit(database.Mutation{
        Entity: schema.EntityImage, ValueID: id,
        Record: []byte("application record"), Projection: projection,
    })
    if err != nil { log.Fatal(err) }
    if err := db.WaitIndexed(ctx, generation); err != nil { log.Fatal(err) }

    engine, err := search.New(db.Index())
    if err != nil { log.Fatal(err) }
    clause, err := engine.Values(tagNamespace, bitmapidx.EnumBytesValue([]byte("f")))
    if err != nil { log.Fatal(err) }
    plan, err := engine.Compile(schema.EntityImage, clause)
    if err != nil { log.Fatal(err) }
    ids, err := plan.AppendIDs(ctx, nil, search.NewWorkspace(128))
    if err != nil { log.Fatal(err) }

    record, err := db.Get(schema.EntityImage, id)
    if err != nil { log.Fatal(err) }
    log.Printf("ids=%v record=%q", ids, record)
}
```

The complete runnable version, including delete/reinsert and close/reopen, is
[`cmd/imageidx/examples/database`](cmd/imageidx/examples/database/main.go).
Earlier examples in `examples/basic`, `examples/service`, and
`examples/bitmaponly` use `index.Store` directly and demonstrate the advanced
standalone API.

## Search

`search.New(db.Index())` binds the current derived store. `Engine.Compile`
prepends the entity liveness restriction and validates namespace ownership.
Plans are reusable; workspaces and destination slices are caller-owned.

```go
engine, _ := search.New(db.Index())
clause, _ := engine.Values(tagNamespace, bitmapidx.EnumBytesValue([]byte("f")))
plan, _ := engine.Compile(schema.EntityImage, clause)
workspace := search.NewWorkspace(1024)
ids, err := plan.AppendIDs(ctx, dst[:0], workspace)
```

`Engine` plans capture one pinned snapshot for each execution. A query sees one
coherent publication generation across its participating indexes, while writers
continue publishing. Direct `search.Compile` plans are lower-level and read live
engine views; keep their indexes open. `search.Executor` pools workspaces for
services without changing plan or result ownership.

## Consistency and lifecycle

- `DB.Commit` durably writes canonical records and ordered journal entries; its
  returned generation is the last committed sequence.
- `DB.WaitIndexed(ctx, generation)` waits until derived indexes are durable and
  visible through that generation. Ordinary searches may lag the latest commit
  until this wait completes.
- `DB.Sync(ctx)` waits for every generation committed before the call.
- `DB.Checkpoint()` checkpoints derived indexes; canonical Pebble data is already
  durable. Run it during idle periods.
- `DB.Close()` stops new operations, drains the coordinator, closes the index,
  and releases locks. It may wait for active query plans to release their pinned
  snapshots; drain long-running reads before closing.

## Ownership and limits

- `DB.Get` returns caller-owned bytes.
- `Mutation.Record` and `Mutation.Projection` are caller-owned and may be reused
  after `Commit` returns.
- `search.Workspace` is exclusive to one execution and must not be shared or
  re-entered from a callback.
- `DB.Index()` is an advanced accessor. Do not write to or close it while the
  coordinator is running.
- `Options.BatchSize` caps one index-apply batch at 64 operations. A `Commit`
  call may contain more mutations, bounded by `MaxPending`, memory, and
  `MaxProjectionBytes`.
- `MaxProjectionBytes` defaults to 1 MiB; `OversizedRetentionBytes` defaults to
  4 MiB. Zero selects the documented defaults.
- `ValueID` zero is reserved. Entity-scoped IDs may overlap across entity kinds.
- Vector `ValueID`s must fit the vector engine's `uint32` NodeID range.

## Platform support

The repository CI currently exercises Linux `amd64` and Linux `arm64`. The
public build, tests, and race suites run on both. Two internal SIMD-kernel
equivalence suites are currently excluded on Linux `amd64`; see
the known CI limitation below. Native Apple Silicon is used for development
benchmarks, not as a CI replacement.

## Version policy

- `schema.dat` container/definitions semantics require a version bump and an
  explicit migration before changing.
- Canonical record versions and journal versions are fail-closed format
  contracts.
- Derived index files are versioned caches. They may be rebuilt or rejected
  when their format is unsupported.
- Stable public API compatibility applies to documented stable APIs. Vector
  APIs and vector formats are experimental for V1.

## Post-V1

The following are intentionally deferred and are **not** V1 blockers unless a
demonstrated correctness or data-loss defect is found:

- per-family parallel writers and a composite root/epoch reclamation redesign;
- selective namespace/family recovery beyond full-index rebuild;
- adaptive batching and broad write-amplification work;
- vector batching, graph redesign, or vector API stabilization;
- new SIMD/SoA architecture or storage allocator changes;
- ranking redesign or new cardinality systems.

See [`AGENTS/`](AGENTS/README.md) for the current architecture and V1 contracts.
Historical design material is available in git history; source, tests, and
package documentation take precedence.

## Known CI limitations

`index/radixhash/internal/tailkernel` and
`index/simhash/internal/hashkernel` currently have AMD64 backend-vs-scalar
equivalence failures and are excluded from the AMD64 CI test/race jobs. They
pass on ARM64, where the AMD64 assembly is not built. This is a known V1
release-gate issue; see
[`AGENTS/CONTRACTS.md`](AGENTS/CONTRACTS.md#release-and-platform-policy).
