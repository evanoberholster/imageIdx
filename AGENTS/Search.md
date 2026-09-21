# Combined Search Implementation

The `search` package is the only package that combines `bitmapidx` fields and
`timeidx`. It compiles conjunctive clauses into a reusable execution plan. Geo
is a `bitmapidx.KindGeo` field; there is no OR, reranking, or vector clause.

## Clauses

- `FieldValues(index, values...)`: every typed value must be present.
- `FieldText(index, text)`: every analyzed token must be present.
- `FieldRange(index, bounds)`: a `KindContinuous` field must fall inside the
  supplied numeric range.
- `During(index, start, end, options...)`: half-open timestamp range. Lossy
  ranges on the same index coalesce by intersection; `ExactTimestamp` remains
  a separate exact verification stage.
- `WithinCells(index, cellIDs...)`: union of S2 cells on a `KindGeo` field.

Clauses are sealed and unknown implementations are rejected. Empty queries
return `ErrEmptyQuery`; invalid or nil field indexes return `ErrInvalidClause`.

## Planning and execution

`Compile` captures field, timestamp, and geo estimates and stably sorts stages
by estimate. Estimates affect work order only. Field stages compile a reusable
`bitmapidx.Query` or numeric range and use the field's collect/filter APIs.

Execution collects the first stage, applies in-place field/exact-timestamp/geo
filters, then collects and intersects remaining stages. Geo collection may use
the workspace-owned transient `roar.MutableBitmap`; this is the only mutable
bitmap used by the cross-index search path. Results are sorted and unique.

`SearchLimit` truncates the sorted prefix. `Count` uses field count operations
for single-field plans and otherwise executes into private scratch. `Iterate`
materializes once. Reusing a plan, destination, and private workspace supports
zero-allocation steady-state execution when capacities are sufficient.

## Workspaces and service

`Workspace` owns intermediate ID scratch and the transient geo accumulator. It
is reusable but not safe for concurrent searches. `Service` pools workspaces
and timestamp write batches; each agent gets a private workspace and result
buffer. Field writes serialize inside their owning `bitmapidx.Index`.
