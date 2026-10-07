# imageIdx

Image indexing in Go: typed bitmap namespaces, timestamp indexes, persistent
vector search, and one query planner that combines them.

Every indexed collection is a `common.Namespace` — a unique number and name
registered in a `common.Schema`. Packages: `common` (shared types),
`bitmapidx` (enum/bool/text/continuous namespaces), `timeidx` (timestamp
ranges), `vectoridx` (HNSW ANN), `simhash` (exact pHash64/PDQ256 similarity),
`geoidx` (exact S2 geographic search), `store` (Pebble key-value collections),
and `search` (combined queries).

## Open an index

### Namespace index (enum, bool, text, continuous)

```go
store, err := bitmapidx.NewStore(
	"./data/namespaces",
	[]bitmapidx.NamespaceSpec{
		{Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum},
		{Namespace: common.Namespace(101), Kind: common.KindBitmapContinuous, Quant: bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1}},
		{Namespace: common.Namespace(102), Kind: common.KindBitmapString, Analyzer: bitmapidx.CodecSpec{Code: bitmapidx.AnalyzerLatin, Version: bitmapidx.AnalyzerVersion}},
	},
	bitmapidx.NamespaceConfig{},
	common.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer store.Close()

tags, _ := store.Lookup(common.NamespaceTag)
focals, _ := store.Lookup(common.Namespace(101))
titles, _ := store.Lookup(common.Namespace(102))
```

Reopen the same store with `bitmapidx.OpenStore(dir, config, durability)`.

### Timestamp index

```go
times, err := timeidx.NewStore(
	"./data/time",
	timeidx.Spec{Namespace: common.Namespace(90), Name: "timeline"},
	timeidx.Config{},
	common.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer times.Close()
```

Reopen with `timeidx.OpenStore(dir, config, durability)`; use
`timeidx.NewMemory(spec, config)` for an in-memory index.

### Vector index

```go
vectors, err := vectoridx.NewStore(
	"./data/vectors",
	vectoridx.Config{
		Namespace:   common.Namespace(100),
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	},
	common.DurabilitySync,
)
if err != nil {
	log.Fatal(err)
}
defer vectors.Close()
```

Reopen with `vectoridx.OpenStore(path, durability)`; use
`vectoridx.NewMemory(config)` for an in-memory index.

### Similarity index (pHash64 + PDQ256)

```go
hashes, err := simhash.Open(
	"./data/simhash",
	simhash.Config{
		MaxShardBytes:   4 << 30,
		SeqBlockRecords: 1024,
		Durability:      common.DurabilityOS,
	},
)
if err != nil {
	log.Fatal(err)
}
defer hashes.Close()
```

Reopen with `simhash.Open(path, config)`. `Close` does not checkpoint; call
`hashes.Checkpoint()` to make pending writes durable.

### Geographic index (S2)

```go
places, err := geoidx.New("./data/geo", geoidx.Config{
	Namespace: common.NamespaceLocation,
})
if err != nil {
	log.Fatal(err)
}
defer places.Close()
```

Reopen with `geoidx.Open(dir, config)`; use `geoidx.NewMemory(config)` for an
in-memory index. Geography is stored as fixed level-19 S2 keys (about 20 m
cells); coordinates become keys with `geoidx.KeyFromLatLng`. `Close` does not
checkpoint; call `places.Checkpoint()` to persist pending mutations.

### Key-value store

`store` is a Pebble-backed collection for values that are not bitmap indexes,
timestamps, vectors, similarity hashes, or geographic locations. The schema is
persisted on creation and verified on every open.

```go
kv, err := store.Open("./data/kv",
	store.Spec{Namespace: common.Namespace(200), Name: "metadata"},
)
if err != nil {
	log.Fatal(err)
}
defer kv.Close()
```

## Add items

### Bitmap namespaces

Stage one or more entries in a reusable `Batch`, `Apply` to queue them, then
`Barrier` to wait until they are WAL-appended and visible. `Remove` wins over
`Add` for the same image and value.

```go
batch := tags.NewBatch()
if err := batch.Add(
	bitmapidx.Entry{ImageID: 100, Value: bitmapidx.EnumStringValue("Trekking")},
	bitmapidx.Entry{ImageID: 200, Value: bitmapidx.EnumStringValue("Travel")},
); err != nil {
	log.Fatal(err)
}
if err := batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err := tags.Barrier(); err != nil {
	log.Fatal(err)
}
```

Other value shapes use the same batch: `batch.Remove(...)` for removals,
`batch.Move(bitmapidx.Move{ImageID, From, To})` for continuous values, and
`batch.ReplaceText(...)` for text. For a continuous value of zero, use
`batch.Add` or `batch.Remove`; `Move` uses zero to mean no value.

```go
batch := focals.NewBatch()
if err := batch.Move(bitmapidx.Move{ImageID: 300, From: 24, To: 50}); err != nil {
	log.Fatal(err)
}
if err := batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err := focals.Barrier(); err != nil {
	log.Fatal(err)
}
```

### Geographic locations

Each `ImageID` has at most one location, stored as a fixed-level S2 key. A move
is `Delete(oldKey, id)` then `Insert(newKey, id)`; the caller supplies `oldKey`
from `imagemetadata`, so the index keeps no reverse lookup.

```go
key, err := geoidx.KeyFromLatLng(37.7749, -122.4194)
if err != nil {
	log.Fatal(err)
}
if err := places.Insert(key, 300); err != nil {
	log.Fatal(err)
}
```

### Timestamps

```go
batch := timeidx.NewBatch(16)
if !batch.Append(timeidx.Insert(100, createdAt)) {
	log.Fatal("batch full")
}
batch.Append(timeidx.Move(200, oldTimestamp, newTimestamp))
batch.Append(timeidx.Delete(300, timestamp))
if err := times.Apply(batch); err != nil {
	log.Fatal(err)
}
batch.Reset()
```

### Vectors

```go
if err := vectors.AddFloat32(100, embedding); err != nil {
	log.Fatal(err)
}
// Delete tombstones the node; re-adding the ID replaces and reconnects it.
if err := vectors.Delete(100); err != nil {
	log.Fatal(err)
}
```

### Similarity hashes

```go
// AppendBatch assigns dense IDs in order and returns the first one.
first, err := hashes.AppendBatch([]simhash.Record{
	{PHash: phash, PDQ: pdq, Quality: quality},
})
if err != nil {
	log.Fatal(err)
}

// UpsertBatch overwrites existing records in place via the batch redo WAL.
if err := hashes.UpsertBatch([]simhash.Update{
	{ImageID: first, Record: updated},
}); err != nil {
	log.Fatal(err)
}
```

### Key-value store

```go
key := store.Key{Namespace: common.Namespace(200), ID: 100}
if err := kv.Set(key, []byte("camera: X100V")); err != nil {
	log.Fatal(err)
}
if err := kv.Delete(key); err != nil {
	log.Fatal(err)
}
```

## Search

### Bitmap namespaces

```go
// MatchValues requires every supplied value (AND).
match, err := tags.MatchValues(bitmapidx.EnumStringValue("Trekking"))
if err != nil {
	log.Fatal(err)
}
ids := match.AppendTo(nil)

travelers, err := tags.ValueIDs(nil, bitmapidx.EnumStringValue("Travel")) // one value, sorted IDs
count, err := tags.CountValues(bitmapidx.EnumStringValue("Trekking"))
```

Continuous namespaces use range queries:

```go
match, err := focals.Range(bitmapidx.Between(24, 70)) // int64 grid bounds
ids = match.AppendTo(nil)
```

Text queries analyze the query and AND its tokens:

```go
match, err := titles.MatchText("golden gate bridge")
```

### Timestamps

```go
results := make([]timeidx.ImageID, 0, 256)
results = times.Search(results, start, end) // half-open [start, end)
```

### Vectors

```go
results, err := vectors.SearchFloat32(query, nil, 10) // nearest 10, squared L2
if err != nil {
	log.Fatal(err)
}
for _, r := range results {
	log.Println(r.ValueID, r.Score)
}
```

### Geographic (S2)

```go
ids, err := places.SearchRadius(nil, 37.7749, -122.4194, 2000) // within 2 km
ids = places.SearchRect(nil, rect)                            // bounding box
ids = places.SearchPolygon(nil, polygon)                      // polygon
near, err := places.Nearest(nil, 37.7749, -122.4194, 10)       // []geoidx.Result
```

For precomputed covers, `places.SearchRanges(dst, ranges)` scans normalized
`geoidx.Range` values. Results are in ascending `ImageID` order.
`geoidx.Result.Distance` is the approximate great-circle distance in meters from
the query to the matched cell center; images in the same S2 cell are tied.

### Similarity (pHash / PDQ)

```go
phMatches := hashes.SearchPHash(nil, phash, 8) // pHash Hamming distance <= 8
pdqMatches := hashes.SearchPDQ(nil, &pdq, 31)  // PDQ Hamming distance <= 31
for _, m := range pdqMatches {
	log.Println(m.ImageID, m.Distance)
}
```

`simhash.Result` carries the exact Hamming `Distance` as `uint8`; results are in
ascending `ImageID` order. The PDQ search radius is clamped to
`simhash.MaxPDQSearchDistance` (128). `hashes.SearchPDQBulk(nil, &pdq, 31)` is
the same query through the SIMD kernel and requires a quiescent store.

### Key-value store

```go
value, err := kv.Get(store.Key{Namespace: common.Namespace(200), ID: 100})
if err != nil {
	log.Fatal(err)
}
log.Println(string(value))

err = kv.Scan(common.Namespace(200), func(key store.Key, value []byte) error {
	log.Println(key.ID, string(value))
	return nil
})
```

### Combined search

Clauses are combined with AND semantics and executed in cardinality order:

```go
plan, err := search.Compile(
	search.AllValues(tags, bitmapidx.EnumStringValue("Trekking")),
	search.During(times, start, end),
	search.InCells(places, cellIDs...),
)
if err != nil {
	log.Fatal(err)
}

matches := make([]search.ImageID, 0, 256)
matches, err = plan.AppendIDs(ctx, matches, search.NewWorkspace(4_096))
```

For a one-off query, `search.SearchOnce(ctx, dst, clauses...)` compiles and
executes immediately.
