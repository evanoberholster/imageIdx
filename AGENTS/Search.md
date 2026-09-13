# Combined Search Implementation

The `search` package is the only package that imports `bitmapidx` and
`timeidx`. It compiles conjunctive clauses into a reusable execution plan.
Geo is not a separate package: geographic clauses target a `bitmapidx`
`KindGeo` field index. There is no `OR`, no builder, no ACL base, no time/geo
ordering, and no `vectoridx` reranking in the current API.

## Clauses

- `AllBitmaps(indexes ...*bitmapidx.Index) Clause`: every predicate must
  contain the image. Multiple clauses merge into one bitmap group. `nil`
  indexes and empty groups are rejected at `Compile` with `ErrInvalidClause`.
- `During(index *timeidx.Index, start, end uint64, options ...TimestampOption)
  Clause`: half-open `[start, end)` range. `start >= end` is valid and
  compiles to an empty plan (`Plan.IsEmpty()`); a `nil` index is
  `ErrInvalidClause`. Lossy-only ranges on the same index coalesce by
  intersection; each `ExactTimestamp` range stays a separate stage.
- `WithinCells(index *bitmapidx.FieldIndex, cellIDs ...uint64) (Clause, error)`:
  union of S2 cells on a `KindGeo` field. Coarser cells expand to level-16
  descendants, finer cells snap to their level-16 ancestor. `nil` index,
  empty cells, non-geo field, and invalid cells return `ErrInvalidClause`
  (joined with the cause). This is the only clause constructor that returns
  an error.
- `ExactTimestamp(lookup timeidx.ExactTimestampFunc) TimestampOption`: verifies
  lossy candidates against canonical data, and is the only timestamp mode
  that can filter in place.

Clauses are sealed (`searchClause()`); unknown implementations are
`ErrInvalidClause`. No clauses is `ErrEmptyQuery`.

## Planning and Execution

`Compile` validates clauses, coalesces bitmap and lossy-timestamp groups,
captures live estimates (`bitmapidx` base cardinality,
`timeidx.EstimateRange`, `FieldIndex.EstimateGeoRanges`), and stably sorts
stages by estimate (ties keep clause order). Estimates affect work order only,
never result semantics. `MustCompile` panics on error. `Plan.Stages()` /
`Explain()` return `{Kind, Estimate}` in execution order.

`searchInto` (`execute.go`) runs: collect `stages[0]`, then all in-place
filters (`canFilter`: bitmap always, timestamp only with exact lookup, geo
always), then collect+intersect for the rest (lossy timestamps). Geo filtering
uses a verify-vs-collect probe bound (`cells * candidates <= 4 * estimate`):
verify via `FilterGeoRanges`, else collect into the workspace-owned
`MutableBitmap` and intersect. Bitmap collects seed from the smallest
`BaseCardinality` index, then `bitmapidx.Filter` in place. Execution
short-circuits to `truncateLimit` once candidates hit zero; cancellation is
checked between stages.

Results are sorted, unique `ImageID`s. `SearchLimit` truncates that sorted
prefix (not a ranking). Bitmap-only `Count` uses `bitmapidx.Count` without
materializing; other counts materialize privately. `Iterate` materializes once,
then yields. `search.Search` is the one-shot compile-and-run path.

Each stage reads one independently loaded immutable view; there is no
database-wide snapshot. `Plan` is immutable after `Compile` and safe to share;
every execution reads current published views. Reader lifetime registration
happens inside the index methods.

## Workspaces and Service

- `Workspace` owns `scratch []ImageID` and a transient `roar.MutableBitmap`
  for geo accumulation. It is reusable but not safe for concurrent searches.
  `NewWorkspace(cap)`, `Reset()` (retains capacity), plus `Len`/`Cap`/
  `EnsureCapacity` helpers. Reusing plan + destination + workspace gives
  zero-allocation steady state when scratch is pre-sized (`Plan.Estimate()`,
  capped at 1<<20 growth per execution).
- `Service` shares one plan across goroutines via pooled workspaces (never
  share a `Workspace`). `NewService(plan, workspaceCap, batchCap)` defaults
  workspace cap to the plan estimate clamped to `[64, 65536]` and batch cap
  to 64. `Prewarm(n)`, `Estimate()`, `IsEmpty()`, `Query`/`QueryContext`,
  `Count`, plus pooled `timeidx` write batches (`AcquireBatch`/`ReleaseBatch`;
  one batch per writer, dropped if still in flight). Writers need no
  service-level lock beyond that rule.

Enum, bool, text, and continuous field indexes are not clause sources; query
them directly and combine in the caller. `vectoridx` is not consulted.
The runnable `cmd/imageidx` example demonstrates one-shot execution and a
long-lived HTTP handler using bitmap + timestamp + geo.
