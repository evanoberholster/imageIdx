# search

Compile conjunctive predicates once into an immutable, shareable `Plan`,
then select an explicit execution operation. Destination buffers and
`Workspace` scratch are exclusively owned by one execution.

## Search usage

Compile once and reuse the plan. `Compile` canonicalizes predicates and
caches cardinality estimates for strategy selection; `SearchOnce` skips
that caching and suits single-use queries only.

```go
plan, err := search.Compile(
    search.AllValues(tags, bitmapidx.EnumStringValue("favorite")),
    search.During(times, start, end),
    search.InCells(locations, cellIDs...),
)
if err != nil { return err }

var ws search.Workspace // reusable scratch; do not share across goroutines
```

With a unified `index.Store`, use `search.New(db)` and `Engine.Compile` to
include entity liveness automatically. For example, after registering a
`KindBitmapBSI` namespace with width 16 in `schema.Definitions`:

```go
engine, err := search.New(db)
if err != nil { return err }
quality, err := engine.BSIRange(40, bitmapidx.ValuesBetween(40, 85))
if err != nil { return err }
plan, err := engine.Compile(schema.EntityImage, quality)
if err != nil { return err }
```

See `cmd/imageidx/examples/bitmaponly` for a runnable unified example.

A full bulk search appends sorted, unique IDs while preserving the
destination prefix. `Search` is the uninstrumented fast path and is
identical to `AppendIDs`.

```go
dst, err := plan.Search(ctx, dst[:0], &ws)
```

`Count` avoids building the ID slice by using cardinality kernels where
possible. `Exists` stops at the first verified match. Both answer
existence questions without paying for full materialization.

```go
n, err := plan.Count(ctx, &ws)
found, err := plan.Exists(ctx, &ws)
```

`FilterIDs` probes an existing candidate list instead of collecting from
the indexes. It preserves input order and duplicates, so
`dst=candidates[:0]` reuse is supported for filtering in place.

```go
kept, err := plan.FilterIDs(ctx, dst[:0], candidates, &ws)
```

`AppendPage` pages by ID cursor: it returns up to `Limit` verified matches
strictly greater than `AfterID`. `Visit` streams matches in the same order
and stops early when the callback returns false.

```go
page, err := plan.AppendPage(ctx, dst[:0], &ws,
    search.Page{AfterID: lastID, Limit: 100})
err = plan.Visit(ctx, &ws, func(id search.ImageID) bool {
    return true // return false to stop early
})
```

For long-lived services, pool workspaces with `Executor` instead of
hand-rolling per-request scratch. The retention budget bounds each idle
workspace, not process memory.

```go
executor, err := search.NewExecutor(search.ExecutorConfig{
    WorkspaceCapacity: 4096,
    MaxRetainedBytes:  8 << 20,
})
ids, err := executor.AppendIDs(ctx, dst[:0], plan)
```

## Contracts and costs

| Operation | Contract |
|---|---|
| `Search` / `SearchWithMetrics` | Same bulk result as `AppendIDs`; metrics are opt-in. |
| `AppendIDs` | Append sorted, unique IDs, preserving the destination prefix. |
| `AppendPage` | Positive limit, exclusive `AfterID`; stop after verified matches. |
| `FilterIDs` | Preserve input order and duplicates; `dst=candidates[:0]` is supported. |
| `Count` | Bitmap cardinality kernels where possible; otherwise verified batches. |
| `Exists` | Stop on the first verified match. |
| `Visit` | Sorted IDs with error reporting and early callback stop. |
| `RankTime` | Top-k by exact `uint64` timestamp gap. |
| `RankGeo` | Top-k by `float64` haversine metres, including fractional metres. |
| `FilterRanked` | Probe supplied entries, preserving order, duplicates and scores. |

## Per-run metrics

`SearchWithMetrics` runs the same bulk search while filling a
caller-owned report. Reuse the report across calls to avoid reallocating
stage records. `Matched` counts pre-ranking matches, `Returned` counts
appended IDs, and `Stages` follows plan order while `ExecutionOrder`
records actual execution (`-1` means unreached).

```go
metrics := search.NewSearchMetrics(plan)
ids, err := plan.SearchWithMetrics(ctx, dst[:0], &ws, metrics)
for _, stage := range metrics.Stages {
    log.Printf("stage=%d kind=%s input=%d passed=%d duration=%s",
        stage.PlanIndex, stage.Stage.Kind, stage.Input,
        stage.Passed, stage.Duration)
}
```

## Predicates and planning

`AllValues` requires every value; `NamespaceText` requires every analyzed
token; `NamespaceRange` uses signed stored-grid bounds; `NamespaceBSIRange`
uses unsigned 16/32-bit BSI bounds (`bitmapidx.ValuesBetween` is inclusive);
`During` uses `[start,end)`
with optional `ExactTimestamp` verification; `InCells` expands S2 cells;
`ExactHash` matches a 256-bit digest in a `radixhash` index. Validation
happens at `Compile`. There are no OR/NOT combinators, except `WithACL`,
which unions user/group bitmaps and intersects the result. Bind it through
the store and authenticate group membership yourself.

```go
plan, err := search.CompileWithStore(store,
    search.WithACL(userID, authenticatedGroupIDs...),
    search.AllValues(tags, bitmapidx.EnumStringValue("favorite")),
)
```

`IsEmpty` is structural only; use `Exists` for live data. Estimates are
planning hints. After the workload shifts, refresh ordering without
repeating canonicalization — but do not refresh per request.

```go
updated, err := plan.Refresh(ctx) // reuses canonicalized predicates
stages := updated.Stages()        // predicate, estimate, capabilities
```

Stages read independently published views: a query is not a
database-wide snapshot. `AfterID` is an ID cursor, not a score cursor.
Keep indexes open during execution and never reuse the active workspace
inside callbacks.

## Exact-hash search

`ExactHash` restricts results to a 256-bit digest held by an exact-content
`radixhash` index (SHA-256 or BLAKE3-256). The digest is compared bit-exactly,
duplicate content can map to several IDs, and the algorithm is a validation
tag plus plan metadata, **not** a check against persisted index metadata;
pass the index intended for that algorithm. For a unified store, use
`engine.ExactHash(ns, algorithm, digest)` followed by
`engine.Compile(entity, clause)` to bind the namespace, verify entity
ownership, and include liveness. Bare `ExactHash(index, ...)` works only with
standalone `Compile`; supply liveness explicitly if needed.

```go
import (
    "crypto/sha256"

    "github.com/evanoberholster/imageIdx/index/radixhash"
    "github.com/evanoberholster/imageIdx/search"
)

digest := sha256.Sum256(content)
plan, err := search.Compile(
    search.ExactHash(shaIndex, search.HashSHA256, digest),
    search.During(times, start, end), // any other conjuncts
)
if err != nil { return err }

ids, err := plan.Search(ctx, nil, &ws)
```

Use `search.HashBLAKE3` with a BLAKE3-256 index. Hashing stays outside the
index: compute the digest with `crypto/sha256` or a BLAKE3 implementation and
pass the raw 32 bytes. A hash stage is a buffered filter: it materializes the
digest's (usually tiny) ID set once and intersects it with the other stages,
so it combines with namespaces, timestamps, geo, and ACLs like any other
clause. Rows deleted at the application layer are not removed from the hash
index — intersect a live or ACL clause when tombstones exist.

## Ranking usage

Native ranking keeps full precision and lower-is-better ordering.
`RankTime` orders by `|timestamp-target|` via an exact-timestamp lookup;
`RankGeo` orders by haversine metres. Missing or invalid entries are
skipped, equal scores break ties by ID, `k < 0` keeps all matches, and
`k == 0` returns `dst` unchanged.

```go
byTime, err := plan.RankTime(ctx, nil, &ws, lookupTime, target, 100)
byGeo, err := plan.RankGeo(ctx, nil, &ws, lat, lng, lookupGeo, 100)
```

`FilterRanked` bridges external vector search: map each vector ValueID to an
ImageID, then probe ANN entries against the plan while preserving source order,
duplicates, and integer scores. Over-fetch from the ANN side because a
selective plan can under-fill.

```go
kept, err := plan.FilterRanked(ctx, nil, &ws, annResults,
    func(r vectoridx.Result) (imageidx.ImageID, bool) { return imageForValue(r.ValueID) },
    func(r vectoridx.Result) uint64 { return uint64(r.Score) })
```

`search/ranking` is the optional `uint16` fusion layer used after
native Top-k, at the presentation boundary. `Rank` is higher-is-better:
`NoRank=0` (missing, sorts last), `MinRank=1` (worst), `MaxRank=65535`
(best). Each lane is monotonic within itself only; lanes are never
directly comparable before fusion.

```go
import "github.com/evanoberholster/imageIdx/search/ranking"

// Encode one lane per signal; missing entries get ranking.NoRank.
timeRanks := make([]ranking.Rank, len(ids))
vecRanks := make([]ranking.Rank, len(ids))
ranking.RankTimeSignal(timeRanks, ids, target, lookup)
for i, sim := range cosineSims {
    vecRanks[i] = ranking.QuantizeCosine(sim)
}

// Fuse heterogeneous lanes with RRF and sort the output.
var sig ranking.Signals
sig.ImageIDs = ids
sig.Values[0], sig.Values[1] = timeRanks, vecRanks
sig.Kinds[0], sig.Kinds[1] = ranking.KindTime, ranking.KindVector
sig.Count = 2

fused := make([]ranking.Rank, len(ids))
var rrf ranking.RRF // reuse per goroutine; holds scratch
if err := rrf.Fuse(fused, &sig); err != nil { return err }

// Full sort, or Top-K when only a few heads are needed.
ranking.Sort(ids, fused)
topIDs, _ := ranking.TopK(nil, nil, ids, fused, 100)
_ = topIDs
```

`Signals` lanes are positional (`ImageIDs[i]` matches `Values[s][i]`)
until after `Fuse`; only final results reorder. All hot paths reuse
caller buffers with zero allocations after warmup. `Results` holds the
same parallel layout for the fused output.

Rank-phase metrics use the shared `search/metrics` report, the same
type as `SearchMetrics.Rank`: pass one `*metrics.RankMetrics` across
the `...WithMetrics` entry points (encode, `Fuse`, `Sort`, `TopK`) and
durations accumulate into `ScoreDuration`/`SortDuration`. Nil reports
are rejected; use the plain entry points for the fast path.

See [PERFORMANCE.md](PERFORMANCE.md) for benchmark methodology and results.
