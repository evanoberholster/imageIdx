# geoidx

`geoidx` is a fixed-level S2 candidate index: one coordinate per image,
bucketed into S2 level-16 cells (`GeoS2Level`). Cells with up to four images
(`geoSparseSlots`) use a fixed-width sparse layout; denser cells reuse
immutable `roar.Bitmap` values. The read representation is split into private
immutable shards, so a publication rebuilds only shards touched by a batch.
Searches load one atomic generation and never inspect writer mutations.

All index packages alias the canonical root `imageidx.ImageID` type.

## Building an index

```go
geo, err := geoidx.New(geoidx.Spec{
	IndexID: 1,
	Entries: []geoidx.Entry{
		{ImageID: 100, Latitude: 48.8566, Longitude: 2.3522},   // Paris
		{ImageID: 200, Latitude: 43.2965, Longitude: 5.3698},   // Marseille
		{ImageID: 300, Latitude: 45.7640, Longitude: 4.8357},   // Lyon
	},
}, geoidx.Config{})
if err != nil {
	log.Fatal(err)
}
defer geo.Close()
```

A coordinate maps to its fixed cell with `geoidx.KeyFromLatLng`, and any S2
cell ID at levels 0 through 16 converts to descendant key ranges with
`geoidx.RangesFromCellIDs`:

```go
key, err := geoidx.KeyFromLatLng(48.8566, 2.3522)
if err != nil {
	log.Fatal(err)
}
ranges, err := geoidx.RangesFromCellIDs(nil, []uint64{uint64(
	s2.CellIDFromLatLng(s2.LatLngFromDegrees(48.8566, 2.3522)).Parent(geoidx.GeoS2Level),
)})
if err != nil {
	log.Fatal(err)
}
_ = key
candidates := geo.SearchRanges(nil, ranges)
```

`SearchRanges` appends matching image IDs to `dst`, allocating only if `dst`
lacks capacity. Matches arrive grouped by cell, not sorted: sort the result
(`slices.Sort` + `slices.Compact`) when order matters.

## Country search: covering a region

`CoverRegion` converts any `s2.Region` (rectangle, cap, polygon, loop) into
sorted, normalized level-16 descendant ranges. A country is naturally a
bounding rectangle; supply a reusable `CoverWorkspace` and a destination with
sufficient capacity for the allocation-free path (a `CoverWorkspace` is not
safe for concurrent use — keep one per query worker).

```go
// Metropolitan France, approximated by its bounding rectangle.
var france s2.Region = s2.RectFromLatLng(
	s2.LatLngFromDegrees(41.3, -5.2),
).AddPoint(s2.LatLngFromDegrees(51.1, 9.6))

workspace := geoidx.NewCoverWorkspace(64)
ranges := make([]geoidx.Range, 0, 64)
results := make([]geoidx.ImageID, 0, 1024)

ranges, err = geo.CoverRegion(ranges[:0], france, geoidx.CoverOptions{
	MaxCells:  32,
	Workspace: workspace,
})
if err != nil {
	log.Fatal(err)
}
results = geo.SearchRanges(results[:0], ranges)
slices.Sort(results)
results = slices.Compact(results)
```

`MaxCells` zero falls back to `Config.MaxCoverCells`. Without a workspace the
coverer allocates scratch per call; with one it never grows during
`CoverRegion`, but `dst` must then have room for the cover
(`ErrCoverBufferFull` otherwise). Allocation behavior inside the region's own
`IntersectsCell`/`ContainsCell` methods remains that implementation's
responsibility.

`EstimateRanges` returns the covered candidate count without materializing
IDs — useful for sizing buffers and for ordering combined-search stages:

```go
if n := geo.EstimateRanges(ranges); uint64(cap(results)) < n {
	results = make([]geoidx.ImageID, 0, n)
}
```

## City search: covering a cap and reusing buffers

A city is naturally a cap around its center. The loop below reuses the same
workspace, ranges, and result buffers on every query, so steady-state city
search performs no allocations:

```go
center := s2.PointFromLatLng(s2.LatLngFromDegrees(48.8566, 2.3522))
var paris s2.Region = s2.CapFromCenterAngle(center, s1.Angle(15.0/6371.0)) // ~15 km

workspace := geoidx.NewCoverWorkspace(64)
ranges := make([]geoidx.Range, 0, 64)
results := make([]geoidx.ImageID, 0, 256)

// Per query:
ranges, err = geo.CoverRegion(ranges[:0], paris, geoidx.CoverOptions{
	MaxCells:  16,
	Workspace: workspace,
})
if err != nil {
	log.Fatal(err)
}
results = geo.SearchRanges(results[:0], ranges)
slices.Sort(results)
results = slices.Compact(results)
```

When candidates already exist — e.g. bitmap or timestamp prefiltering narrowed
the set — verify them against the covered cells with `FilterRanges` instead of
materializing the whole population. Matches arrive grouped by cell, so re-sort
afterwards. As a rule of thumb, verification wins while
`cells × candidates <= 4 × population` (`CountCells` walks key tables only);
beyond that, collect and intersect.

```go
// candidates holds sorted IDs from an earlier stage.
filtered := geo.FilterRanges(results[:0], candidates, ranges)
slices.Sort(filtered)
```

`CollectRanges` offers a third shape: it folds covered cells into a caller
reset `*roar.MutableBitmap` (dense cells fold without decoding to IDs) for a
single sorted, unique materialization:

```go
m := roar.NewMutableBitmap()
m.Reset()
geo.CollectRanges(m, ranges)
sorted := m.AppendTo(nil)
```

## Combined search

The `search` package compiles geographic restrictions together with bitmap and
timestamp clauses under AND semantics. Geographic input is S2 cell IDs, not
coordinates: `search.WithinCells` accepts cells at levels 0 through 16 and
normalizes them to level-16 descendant ranges. Exact polygon or coordinate
verification belongs in the caller's canonical-data layer.

```go
// One level-10 cell over Paris (~10 km across) plus a tag restriction.
cell := uint64(s2.CellIDFromLatLng(
	s2.LatLngFromDegrees(48.8566, 2.3522),
).Parent(10))
geoClause, err := search.WithinCells(geo, cell)
if err != nil {
	log.Fatal(err)
}
plan, err := search.Compile(
	search.AllBitmaps(paris, favorites),
	geoClause,
)
if err != nil {
	log.Fatal(err)
}

// Plans and workspaces are reusable. Search reads the indexes' current
// published views; compiled estimates affect stage ordering only.
workspace := search.NewWorkspace(4096)
matches := make([]search.ImageID, 0, 256)
matches = plan.Search(matches, workspace)
```

For a one-off query, pass clauses directly to `search.Search`. With sufficient
capacities the combined path performs zero heap allocations.

## Writes

Writes are explicit cell moves; `S2Key(0)` is the absent sentinel. There is no
reverse image-to-cell map: the caller supplies both ends.

```go
lyon, err := geoidx.KeyFromLatLng(45.7640, 4.8357)
if err != nil {
	log.Fatal(err)
}
bordeaux, err := geoidx.KeyFromLatLng(44.8378, -0.5792)
if err != nil {
	log.Fatal(err)
}
err = geo.Apply([]geoidx.Mutation{
	{ImageID: 400, To: lyon},              // insert: From zero means absent
	{ImageID: 300, From: lyon, To: bordeaux}, // move
	{ImageID: 200, From: keyMarseille},    // removal: To zero
})
if err != nil {
	log.Fatal(err)
}
```

`Apply` validates and queues one atomic batch. Small writes become visible at
the configured `PublishThreshold`/`PublishInterval`; `Flush` folds pending
moves into replacement immutable shards immediately and is the explicit
visibility boundary. Only cells touched by a batch are recomputed, so small
flushes stay proportional to the batch, not the index. `Apply` and `Flush` are
safe for concurrent use; readers always observe one atomic generation.

## Persistence

```go
geo, err := geoidx.NewPersistent(dir, spec, geoidx.Config{}, geoidx.DurabilityOS)
if err != nil {
	log.Fatal(err)
}
defer geo.Close()

if err := geo.Checkpoint(); err != nil {
	log.Fatal(err)
}
if err := geo.Vacuum(); err != nil {
	log.Fatal(err)
}
```

Reopen with `geoidx.OpenPersistent`, which maps the published generation and
replays the WAL tail. `Checkpoint` publishes a new generation and manifest;
`Vacuum` checkpoints and removes obsolete generations. Use
`geoidx.DurabilitySync` when each WAL append must reach the disk. Storage uses
checksummed, mmap-oriented generation files with zero-copy bitmap payloads.

## Nearest images

`Nearest` returns the exact top K by haversine distance for images whose
canonical coordinates come from the caller's lookup (typically the same
database column that fed `Entry` values):

```go
lookup := func(id geoidx.ImageID) (float64, float64, bool) {
	lat, lng, ok := db.Coordinates(id)
	return lat, lng, ok
}
nearest := geo.Nearest(nil, 48.8566, 2.3522, 10, lookup)
for _, r := range nearest {
	fmt.Println(r.ImageID, r.DistanceMeters)
}
```

## Concurrency and configuration

- Reads are lock-free; `Close` waits for reads referencing retired state.
- One `CoverWorkspace` per query worker; never share it across goroutines.
- `Config{PublishThreshold, PublishInterval, MaxCoverCells}` zero-values to
  256 pending moves, 25ms, and 32 cover cells. Negative values are rejected
  with `ErrInvalidConfig`.
- `Metadata` reports the current generation plus sparse/dense cell counts.
