# Phase 3 Writer Protocol Decision

**Baseline:** `c466464` (Phase 3.0 merged)  
**Status:** alternative gate invoked for Phase 3.1; lock-free query snapshots
implemented for 3.3

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
   generations to existing queries. The approved safe alternative is an
   optimistic query snapshot: capture every stage's engine view between two
   writer-sequence boundaries, retry if a writer overlaps the capture, and pin
   the captured views with reader tokens for the query lifetime.

## Decision

Phase 3.1 invokes the documented alternative gate:

- Keep the existing **one serialized index apply writer** and one global
  publication/acknowledgment authority.
- Do not introduce independently visible per-family writers.
- Do not claim a family-writer throughput win.
- Preserve all current crash, replay, source-generation and recovery semantics.
- Continue with write-amplification reductions that are safe within the
  serialized coordinator.
- Use the 3.3 optimistic snapshot protocol for lock-free query reads; do not
  block writers for ordinary query execution.

## Why not a partial family-worker implementation

Parallelizing only SimHash and keeping the other engines serialized would still
require a composite root before queries could be coherent. Publishing SimHash
early while bitmap/time/geo/radix remain behind would expose mixed generations.
A family worker therefore needs the same staged-generation infrastructure as
the full design; a partial implementation would be unsound.

## 3.3 lock-free query snapshot protocol

The implemented protocol is:

1. `Store.writeSeq` is even while no writer is publishing. A writer increments
   it to an odd value before the first engine mutation and increments it again
   after the last publication.
2. `Plan.beginQuery` waits for an even sequence, captures a shallow plan copy,
   and replaces every engine handle with a reader-pinned `Snapshot` of that
   engine's current view.
3. The sequence is re-read after all snapshots are captured. If it changed, all
   pins are released and the capture is retried. Context cancellation is checked
   between attempts.
4. Query execution reads only the pinned handles. Writers can publish newer
   engine views without waiting for the query; readers see exactly the captured
   generation.
5. Snapshot pins are released on every return path. Engine reader trackers keep
   mmap regions and retired allocators alive until those pins drain.

Engine reclamation may still wait for a pinned snapshot before unmapping old
mmap regions. That is bounded backpressure on storage retirement, not a read
lock, and does not change the query's visible generation. Deferred reclamation
is the 3.5 follow-up.

## 3.1 scope after the gate

Safe serialized-path work only:

- deduplicate genuinely redundant staged operations where engine semantics
  prove last-write-wins;
- avoid empty/no-op engine flushes and unnecessary manifest publication where
  source-generation semantics permit;
- preserve existing publication ordering, reader visibility and crash tests;
- benchmark against the Phase 3.0 baseline and report any gains or regressions.

The 15% family-writer throughput target is not claimed by this alternative.
