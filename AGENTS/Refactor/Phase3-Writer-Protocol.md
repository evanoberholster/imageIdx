# Phase 3 Writer Protocol Decision

**Baseline:** `c466464` (Phase 3.0 merged)  
**Status:** alternative gate invoked for Phase 3.1

## Required invariant

A query must observe either the complete publication before a journal interval or
the complete publication after it. A family worker may not publish a state that
can be observed while another family still represents the previous interval.

## Findings against the current code

1. `index.Store.applyBatch` is the single serialized publisher. It stages typed
   operations, flushes each engine in a fixed order, advances source-generation
   watermarks, and writes the unified manifest.
2. Each engine currently owns its own reader-visible root:
   - `bitmapidx` publishes namespace/store views;
   - `timeidx` swaps `atomic.Pointer[snapshot]`;
   - `geoidx` swaps an `atomic.Pointer[view]`;
   - `radixhash` swaps an `atomic.Pointer[indexView]`;
   - `simhash` swaps its dense and sparse views.
3. Queries obtain raw engine handles through `index.Store.Bitmap/Time/Geo/Hash/Sim`
   and then call engine-local readers. There is no composite root containing the
   coherent engine snapshots for one source generation.
4. `storage.ReaderTracker` is a two-slot stop-the-world reader drain. It can
   protect retirement, but it cannot retroactively make independently loaded
   engine views belong to one generation.
5. Making per-family writers publish independently would expose mixed
   generations to existing queries. Making them safe requires one of:
   - new staged generation APIs for every engine, plus a composite root held by
     every query for its full lifetime; or
   - a global query gate that blocks readers while any family publishes, which
     violates the Phase 3 reader/writer requirement and creates unacceptable
     ingestion latency.

The first option is a broad engine and query-API redesign. The second is not an
acceptable implementation of the approved concurrency model.

## Decision

Phase 3.1 invokes the documented alternative gate:

- Keep the existing **one serialized index apply writer** and one global
  publication/acknowledgment authority.
- Do not introduce independently visible per-family writers.
- Do not claim a family-writer throughput win.
- Preserve all current crash, replay, source-generation and recovery semantics.
- Continue with write-amplification reductions that are safe within the
  serialized coordinator, and with the planner/read-root/recovery phases after a
  separate design for composite snapshots.

## Why not a partial family-worker implementation

Parallelizing only SimHash and keeping the other engines serialized would still
require a composite root before queries could be coherent. Publishing SimHash
early while bitmap/time/geo/radix remain behind would expose mixed generations.
A family worker therefore needs the same staged-generation infrastructure as
the full design; a partial implementation would be unsound.

## Required follow-up design for 3.3

The composite root design must define:

- a root descriptor containing immutable engine snapshot references and one
  source generation;
- a query pin acquired before loading the root and held through all clause and
  result assembly;
- safe retirement of mmap regions, segments and lookup tables until all pins
  from the prior root have drained;
- bounded retention for long readers and documented backpressure;
- a crash-safe durable prepare/manifest/activation path before the root swap.

Until that design exists, the serialized writer remains authoritative.

## 3.1 scope after the gate

Safe serialized-path work only:

- deduplicate genuinely redundant staged operations where engine semantics
  prove last-write-wins;
- avoid empty/no-op engine flushes and unnecessary manifest publication where
  source-generation semantics permit;
- preserve existing publication ordering, reader visibility and crash tests;
- benchmark against the Phase 3.0 baseline and report any gains or regressions.

The 15% family-writer throughput target is not claimed by this alternative.
