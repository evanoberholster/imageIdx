# Bitmap namespaces

`index/bitmapidx` owns typed posting namespaces and BSI (bit-sliced) numeric
values. Define each namespace in `schema.Definitions` with `index.BitmapDef`
before creating a unified `index.Store` or database. `KindBitmapEnum`,
`KindBitmapBool`, and `KindBitmapString` support value/token queries;
`KindBitmapContinuous` supports signed grid ranges. Geography uses the separate
`index/geoidx` engine.

`KindBitmapBSI` stores one unsigned `uint16` or `uint32` per ID, including zero.
Use `bitmapidx.Uint16Value`/`Uint32Value` with unified `index.Entry.Bitmaps`,
or the typed index's `Set`/`BSIMove`. Use `bitmapidx.ValuesBetween` (inclusive),
`ValuesAtLeast`, or `ValuesAtMost` for BSI queries. `BSIRangeQuery`,
`BSICountRange`, and `BSIFilterRange` are the native collect, count, and
candidate-filter paths. Bounds outside the namespace width return an error.
`search.NamespaceBSIRange` and `search.Engine.BSIRange` combine these with
other clauses while preserving entity liveness. Search plans compiled through
`search.Engine` execute against one pinned snapshot; direct plans read live
views.

Readers use immutable views; writes serialize per engine. Reuse search plans,
workspace buffers, and candidate filters rather than materializing every
posting. For detailed behavior and tests, see `index/bitmapidx/bsi.go`,
`index/bitmapidx/bsi_test.go`, and `index/bsi_integration_test.go`.
