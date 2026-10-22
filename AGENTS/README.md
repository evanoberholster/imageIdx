# Agent documentation

The repository-root [`AGENTS.md`](../AGENTS.md) is the agent workflow contract.
This directory contains architecture notes, design history, and subsystem
references. It is not a second public API specification.

For current behavior, precedence is:

1. source and tests;
2. package `README.md` files and Go package documentation;
3. this directory’s current architecture notes;
4. design proposals and historical reports.

## Current architecture and workflow

| Document | Status | Notes |
|---|---|---|
| [`BuildConcepts.MD`](BuildConcepts.MD) | Current | Package map, consistency, validation |
| [`BitmapIndex.md`](BitmapIndex.md) | Current | Bitmap/BSI namespace guide |
| [`Search.md`](Search.md) | Current | Search planning and snapshot contract |
| [`Bench.MD`](Bench.MD) | Current methodology | Benchmark rules; some engine names are historical |
| [`KVStore.md`](KVStore.md) | Current | Advanced `store` guide |
| [`Schema.md`](Schema.md) | Current | Schema/ID guide; authoritative format lives in `schema/` |

## Subsystem design notes

These documents explain engine design and may contain implementation history.
Verify exact APIs against the package docs and source.

- [`RoarFormat.md`](RoarFormat.md) — bitmap serialization
- [`BitmapKernel.MD`](BitmapKernel.MD) — SIMD/bitmap kernels
- [`SimHash.md`](SimHash.md) — perceptual hash store
- [`Timeidx.md`](Timeidx.md) — timestamp index
- [`RadixTreeHash.MD`](RadixTreeHash.MD) — exact hash index
- [`Ranking.MD`](Ranking.MD) — optional ranking/fusion
- [`VectorIdx.MD`](VectorIdx.MD) — **experimental** vector design

## Historical proposals

These are retained for design context only. They do not describe the current
implementation and must not be used as API guidance.

- [`CryptoHash.MD`](CryptoHash.MD) — retired exact-hash proposal; use
  `index/radixhash`.
- [`HashIdx2.MD`](HashIdx2.MD) — retired hash-index proposal; use
  `index/radixhash` and `index/simhash`.
- [`Refactor/`](Refactor/) — Phase 1–3 working material, benchmark evidence, and
  the active V1 plan; see [`Refactor/README.md`](Refactor/README.md).

## Authoritative package documentation

- [`database/README.md`](../database/README.md) — primary V1 lifecycle and
  search integration.
- [`database/FORMAT.md`](../database/FORMAT.md) — canonical, journal, key, and
  `schema.dat` compatibility.
- [`schema/README.md`](../schema/README.md) and
  [`schema/FORMAT.md`](../schema/FORMAT.md) — namespace definitions and IDs.
- [`index/README.md`](../index/README.md) — unified/standalone index store.
- [`search/README.md`](../search/README.md) — plans, workspaces, snapshots.
- [`store/README.md`](../store/README.md) — advanced KV primitive.
- [`index/vectoridx/README.md`](../index/vectoridx/README.md) — experimental
  vector API.

## Terminology corrections

The old `common` package name is retired. Current code uses `schema` for
namespace definitions, entity identity, and ValueIDs. Do not add a new
`common` package or a parallel schema registry.

Derived indexes are rebuildable caches. `schema.dat` and canonical Pebble
records are the durability authority. Search plans compiled by `search.Engine`
pin one coherent snapshot per execution; direct `search.Compile` plans retain
live-view semantics.
