## Summary

- Replaces `store.Store`’s KV namespace membership map with an unexported 8 KiB bitmap. `schema.Definitions` order, `schema.dat` bytes, journal encoding and index formats are unchanged.
- Adds a reusable coordinator workspace with stable 64 KiB projection blocks, one bounded oversized buffer, fixed-width queued mutation descriptors, and a single contiguous copy of each journal before+after payload.
- Replaces the O(N×M) projection diff with typed deterministic sort-and-merge, including explicit exhausted-side handling, duplicate compaction, and NaN/signed-zero consistency.
- Adds safe bounded same-entity coalescing. SimHash, vector, and delete→reinsert groups fall back to ordered sub-batches.
- Adds lightweight cumulative writer statistics for logical coordinator operations.

Follow-up additions:

- Added real journal-replay/reopen coverage for insert→update coalescing with an unchanged camera association plus a changed rating association, using both `Decoder` and `DecodeInto`.
- Added sequence, liveness, empty-projection, randomized 64-record sequential-vs-coalesced and coalesced crash-boundary tests.
- Added workspace payload boundaries (0/1/65535/65536/65537 bytes), malformed payload rejection, ordinary/oversized reuse, and namespace bitmap boundary/reopen tests.
- Added a set-correct small-diff fast path for one-association updates.

## API changes

- Added `database.Options.MaxProjectionBytes` (default 1 MiB) and `OversizedRetentionBytes` (default 4 MiB).
- Added optional `database.Options.DecodeInto` with `database.DecodeIntoFunc` and `database.DecodeScratch`. The existing `database.Decoder` remains supported.
- Added `DB.Stats()`, `store.Store.Stats()` and `index.Store.Stats()`. These are application-level counters and logical byte estimates, not device-write or `fsync` measurements.
- Preserved `database.Commit` duplicate `(Entity, ValueID)` rejection, the 64-mutation batch cap, and all existing error behavior.

## Performance

- KV membership: map ≈ 5.6–6.1 ns/op; bitmap ≈ 0.25 ns/op; both 0 allocs. Retained cost: +8 KiB per `store.Store`.
- Projection diff, one changed association: 121 ns → 45.7 ns, 0 allocs (after the small-diff fast path).
- Projection diff, 4 associations: 216 ns → 146.6 ns, 0 allocs.
- Projection diff, 16 associations: 633 ns → 550 ns, 0 allocs.
- Projection diff, 256 associations: 31.8 µs → 30.8 µs, 0 allocs.
- Cold vs warm commit+index: batch 1 16.6 KiB → 9.7 KiB B/op; batch 16 38.3 KiB → 30.3 KiB; batch 64 109.4 KiB → 99.4 KiB. The difference is the one-time coordinator workspace and first projection block.
- End-to-end throughput remains within roughly ±4% of baseline.
- Pebble `SetDeferred` was benchmarked (414 ns vs 531 ns for 64 raw records) but not adopted because the generic database does not know application-encoded lengths.

## Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate` (passes with a context-coverage warning)
- CI runs Linux AMD64 and ARM64 only; macOS runners are no longer used.
- Escape analysis reported only error/panic formatting and scratch-growth escapes; warmed projection/workspace paths do not allocate per entry.
