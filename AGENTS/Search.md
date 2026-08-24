# Combined Search Implementation

The `search` package is the only package that imports `bitmapidx`, `timestamp`,
and `geoindex`. It compiles conjunctive clauses into a reusable execution plan.

## Clauses

- `AllBitmaps(indexes...)`: every predicate must contain the image.
- `During(index, start, end, options...)`: compressed timestamp range, with
  optional `ExactTimestamp` canonical verification.
- `WithinCells(index, cellIDs...)`: geographic union expressed as valid S2
  cell IDs at levels 0 through 16.

Clauses are sealed so invalid internal combinations cannot be constructed.
Empty queries return `ErrEmptyQuery`; nil or empty clauses return
`ErrInvalidClause`.

## Planning and Execution

`Compile` coalesces all bitmap clauses, captures live upper-bound estimates,
and stably sorts stages by estimate. Ordering affects work only, never result
semantics. A bitmap stage can filter candidates in place. An exact timestamp
stage can also filter in place. Other stages collect into workspace scratch
and intersect sorted unique IDs.

`Plan.Search(dst, workspace)` appends sorted unique matches. Reusing the plan,
destination, and a sufficiently sized `Workspace` gives zero heap allocations
on the combined search path. `search.Search` is the one-shot convenience path
that compiles and runs immediately.

Each stage reads one immutable view from its owning index. The plan does not
provide a database-wide transactional snapshot. All packages share the same
`imageidx.ImageID` alias; no conversion or copy is required at package
boundaries. Read-side lifetime registration is performed inside the index
methods; neither `Plan` nor its caller owns per-query guards.

The runnable `cmd/imageidx` example demonstrates both one-shot execution and a
long-lived HTTP handler using all three index families.
