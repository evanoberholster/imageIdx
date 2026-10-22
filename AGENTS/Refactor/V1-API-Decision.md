# V1 API decision record

**Baseline inspected:** `main` at `c466464` (Phase 3.0) and PR #27 at
`12260ac` (Phase 3.1–3.5, all CI checks green).

This record freezes the intended V1 public surface for stabilization. It is not
a new architecture proposal.

## Primary lifecycle

`database.DB` is the documented V1 application entry point for:

- open/reopen;
- commit and canonical retrieval;
- indexing waits and checkpoint maintenance;
- closing;
- access to the derived index for search integration.

The current bootstrap still requires one standalone `index.Create` call before
the first `database.Open`. V1 documents this as an explicit one-time creation
step. A future `database.Create` wrapper would be a convenience change, not a
persistence change; it is not required to freeze the current contract.

Applications bind search with `search.New(db.Index())`. `DB.Index()` is an
advanced accessor; callers must not write to or close it while the coordinator
runs.

## Stable V1 surface

| Package | Stable surface |
|---|---|
| `database` | `DB`, `Open`, `Mutation`, `Options`, `CanonicalProjectionMode`, `RecoveryPolicy`, `RecoveryStatus`, stable error sentinels, `Commit`, `WaitIndexed`, `Sync`, `Checkpoint`, `Get`, `DatabaseGeneration`, `IndexedGeneration`, `Close` |
| `schema` | `Definitions`, `NamespaceDef`, `EntityKind`, `Namespace`, identifier aliases, `Schema` open/ID allocation APIs, durable format errors |
| `search` | `Engine`, `Plan`, `Executor`, clause constructors, `Workspace`, `Page`, result types, metrics, stable errors |
| `store` | Advanced stable KV `Store`, `Key`, `Batch`, `Spec`/`Open`, sync durability, owned `Get`, scan ownership |
| `index` | Advanced stable unified store, typed namespace accessors, `Entry`, `Change`, `Batch`, matcher, definition helpers, recovery/health diagnostics |
| `index/bitmapidx` | Advanced stable namespaces and BSI APIs |
| `index/timeidx` | Advanced stable timestamp APIs |
| `index/geoidx` | Advanced stable geographic APIs |
| `index/radixhash` | Advanced stable exact-hash APIs |
| `index/simhash` | Advanced stable perceptual APIs, including sparse external IDs |
| `roar` | Advanced stable immutable bitmap representation/operators |
| `search/ranking` | Optional advanced ranking/fusion helpers |

## Experimental

| Surface | Scope |
|---|---|
| `index/vectoridx` | Public API, performance, and file formats are experimental in V1 |
| Vector portions of `index` | `KindVector`, `VectorDef`, `VectorValue`, `DB`/index accessors and search bridging; functional but not a V1 stability promise |

Vector functionality must remain correct and tested, but no new batching, graph
redesign, or vector API stabilization work is required for V1.

## Search decision

No `database.DB.Search` method is added in V1 unless a benchmark proves it can
preserve the existing explicit `Plan`/`Workspace`/destination contract without
material overhead. The supported high-performance path is:

```go
engine, err := search.New(db.Index())
plan, err := engine.Compile(entity, clauses...)
ids, err := plan.AppendIDs(ctx, dst, workspace)
```

An `Engine` execution pins one coherent snapshot. Direct `search.Compile` plans
remain live-view plans; this distinction must be documented everywhere.

## Persistence decision

- `schema.dat` container v3 is the V1 schema/ID baseline. V2 is read and upgraded
  on publication; v1 allocation formats fail closed.
- Canonical inline version 1, external version 2, journal v2, and store keys are
  frozen as documented in `database/FORMAT.md`.
- Derived index files are rebuildable caches. They are versioned and rejected or
  rebuilt, not promised to be permanently byte-stable.
- Full-index recovery is the supported recovery unit. Selective namespace/family
  recovery is post-V1.

## Ownership and concurrency summary

- `Commit` inputs are caller-owned and reusable after return.
- `Get` returns owned bytes.
- `Workspace`, batches, metrics reports, and plans have documented single-owner
  or concurrent-use rules in package docs.
- `Close` rejects new operations and drains the coordinator; active query
  snapshots may delay index reclamation.
- Writers are serialized. Queries through `Engine` are lock-free with respect to
  writer publication; engine reclamation may backpressure an old mapping.

## Documentation/verification corrections required

1. Root README and package docs must consistently name `database.DB` as primary
   and describe the index bootstrap exception.
2. Search docs must distinguish `Engine` snapshots from live direct plans.
3. Vector docs must say experimental, not “V1 scope.”
4. Sparse SimHash support must be reflected in package/root docs.
5. Canonical and journal formats need `database/FORMAT.md` and fixture coverage.
6. Linux AMD64 CI currently excludes two internal SIMD-kernel equivalence suites.
   That must be fixed or explicitly scoped out of the supported platform matrix
   before a V1 release tag.
7. V1 benchmarks must include `Engine`/database search paths, not only direct
   `Compile` plans.

## Recommendation

Proceed with documentation and format stabilization on this branch. Do not claim
a V1 tag until the AMD64 kernel gate and the stale public contracts are closed.
