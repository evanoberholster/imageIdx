# imageIdx

imageIdx is a persistent, schema-aware image indexing and search library for Go.
It stores authoritative application records in Pebble and derives bitmap,
timestamp, geographic, exact-hash, perceptual-hash, and vector indexes from
canonical projections.

This is the 0.5 documentation baseline. ImageIdx 0.5 is a pre-1.0 release:
the documented API may still change, so pin versions and read release notes
before upgrading. The primary entry point is [`database.DB`](database/README.md).
Lower-level `index`, `store`, `schema`, and `search` packages remain available
for advanced and standalone use.

## Start here

- [Database integration](database/README.md) — canonical records, commits,
  indexing waits, search integration, recovery, and public contracts.
- [Database on-disk format](database/FORMAT.md) — 0.5 canonical, journal, key,
  and schema compatibility rules.
- [Schema and entity IDs](schema/README.md) — namespace definitions and durable
  `schema.dat` allocation.
- [Unified index](index/README.md) — standalone/advanced index store.
- [Search](search/README.md) — reusable plans, workspaces, and execution.
- [Text search with external embeddings](#text-search-with-external-embeddings) —
  image/text vector search without an imageIdx dependency on an ML runtime.
- [0.5 contracts](AGENTS/CONTRACTS.md) — public, advanced, and experimental API
  classification, limits, and release policy.

## Package map

| Package | Role | 0.5 status |
|---|---|---|
| `database` | Authoritative Pebble records, mutation journal, derived-index coordination, recovery | **Primary** |
| `schema` | Namespace definitions, entity identity, durable ValueID allocation | **Primary** |
| `search` | Conjunctive plans, workspaces, executors, ranking helpers ([guide](search/README.md)) | **Primary** |
| `index` | Unified index store and namespace routing ([guide](index/README.md)) | Advanced |
| `index/bitmapidx` | Enum, bool, text, continuous, and BSI bitmap namespaces | Advanced |
| `index/timeidx` | Exact timestamp index | Advanced |
| `index/geoidx` | S2 geographic index | Advanced |
| `index/radixhash` | Exact 256-bit hash index | Advanced |
| `index/simhash` | pHash/PDQ perceptual similarity store | Advanced |
| `index/vectoridx` | HNSW vector index | **Experimental** |
| `store` | Generic Pebble-backed KV collection ([guide](store/README.md)) | Advanced |
| `roar` | Immutable roaring-bitmap representation and operators | Advanced |
| `search/ranking` | Optional ranking/fusion helpers | Advanced |
| `internal/*` | Private storage, kernels, benchmarks | Internal |
| `cmd/imageidx` | CLI, HTTP demo, and examples | Examples/tools |

Vector APIs, vector file formats, vector performance, and vector compatibility
are **experimental** in 0.5. They remain functional and tested, but are not part
of the 0.5 compatibility promise.

## Durable and derived data

There are three distinct durability layers:

1. **`schema.dat`** is the authoritative schema, entity/namespace model,
   projection-version store, and durable entity ID allocator.
2. **Pebble canonical records and the mutation journal** are the authoritative
   application data. A successful `DB.Commit` writes both durably before it
   returns.
3. **Derived indexes** are rebuildable projections. Index files are versioned and
   fail closed when unsupported or corrupt; they are not promised to be
   byte-compatible forever. `database` rebuilds the complete index from
   canonical projections when recovery requires it.

No pre-0.5 experimental database is guaranteed to open. 0.5 databases must reopen
with 0.5; unsupported container, journal, canonical, or index versions are
rejected explicitly rather than reinterpreted.

## Quick start

Create or open a database with one root directory. Canonical records live in
`<root>/data` and every derived index lives in the shared `<root>/index`
directory. On a new root, `Options.Definitions` builds the schema and index tree
automatically; later opens reuse `schema.dat` and the existing indexes.

```go
package main

import (
    "context"
    "encoding/binary"
    "log"

    "github.com/evanoberholster/imageIdx/database"
    "github.com/evanoberholster/imageIdx/index"
    "github.com/evanoberholster/imageIdx/index/bitmapidx"
    "github.com/evanoberholster/imageIdx/schema"
    "github.com/evanoberholster/imageIdx/search"
)

const tagNamespace schema.Namespace = 101

func decodeProjection(p []byte) (index.Entry, error) {
    if len(p) != 9 {
        return index.Entry{}, database.ErrCorruptJournal
    }
    return index.Entry{
        ValueID: binary.BigEndian.Uint64(p[:8]),
        Bitmaps: []index.BitmapValue{{
            Namespace: tagNamespace,
            Value:     bitmapidx.EnumBytesValue(p[8:9]),
        }},
    }, nil
}

func main() {
    ctx := context.Background()
    defs := schema.NewDefinitions()
    tag, err := index.BitmapDef(bitmapidx.NamespaceSpec{
        Namespace: tagNamespace, Kind: schema.KindBitmapEnum,
    }, "tags", schema.EntityImage)
    if err != nil { log.Fatal(err) }
    if err := defs.Register(tag); err != nil { log.Fatal(err) }

    db, err := database.Open("./db", decodeProjection, database.Options{
        Definitions: defs,
    })
    if err != nil { log.Fatal(err) }
    defer db.Close()

    id := schema.ImageID(100)
    projection := make([]byte, 9)
    binary.BigEndian.PutUint64(projection[:8], uint64(id))
    projection[8] = 'f'
    generation, err := db.Commit(database.Mutation{
        Entity: schema.EntityImage, ValueID: id,
        Record: []byte("application record"), Projection: projection,
    })
    if err != nil { log.Fatal(err) }
    if err := db.WaitIndexed(ctx, generation); err != nil { log.Fatal(err) }

    engine, err := search.New(db.Index())
    if err != nil { log.Fatal(err) }
    clause, err := engine.Values(tagNamespace, bitmapidx.EnumBytesValue([]byte("f")))
    if err != nil { log.Fatal(err) }
    plan, err := engine.Compile(schema.EntityImage, clause)
    if err != nil { log.Fatal(err) }
    ids, err := plan.AppendIDs(ctx, nil, search.NewWorkspace(128))
    if err != nil { log.Fatal(err) }

    record, err := db.Get(schema.EntityImage, id)
    if err != nil { log.Fatal(err) }
    log.Printf("ids=%v record=%q", ids, record)
}
```

The complete runnable version, including delete/reinsert and close/reopen, is
[`cmd/imageidx/examples/database`](cmd/imageidx/examples/database/main.go).
Earlier examples in `examples/basic`, `examples/service`, and
`examples/bitmaponly` use `index.Store` directly and demonstrate the advanced
standalone API.

## Search

`search.New(db.Index())` binds the current derived store. `Engine.Compile`
prepends the entity liveness restriction and validates namespace ownership.
Plans are reusable; workspaces and destination slices are caller-owned.

```go
engine, _ := search.New(db.Index())
clause, _ := engine.Values(tagNamespace, bitmapidx.EnumBytesValue([]byte("f")))
plan, _ := engine.Compile(schema.EntityImage, clause)
workspace := search.NewWorkspace(1024)
ids, err := plan.AppendIDs(ctx, dst[:0], workspace)
```

`Engine` plans capture one pinned snapshot for each execution. A query sees one
coherent publication generation across its participating indexes, while writers
continue publishing. Direct `search.Compile` plans are lower-level and read live
engine views; keep their indexes open. `search.Executor` pools workspaces for
services without changing plan or result ownership.

## Text search with external embeddings

Text search is normal vector search: embed the text, then query the vector
namespace. `imageIdx` does not import an ML runtime and does not need to. The
application owns model loading, tokenization, preprocessing, and model version;
`imageIdx` only receives `[]float32`.

Use a paired image/text encoder. For example, `imageml`'s
`github.com/evanoberholster/imagemL/siglip2` package has paired 768D SigLIP2
image and text towers. DINOv3 is image-only and cannot answer a text query
because it has no matching text tower.

Keep the encoder in your application package. This interface is enough:

```go
type Embedder interface {
    Dim() int
    EmbedImageIntoContext(context.Context, []float32, image.Image) error
    EmbedTextIntoContext(context.Context, []float32, string) error
}
```

`*siglip2.Embedder` satisfies it directly. Do not add `imageml` to the
`imageIdx` module; a service or a separate adapter module can import both.

### 1. Register a vector namespace

One namespace holds one embedding model/space. For a 768D normalized SigLIP2
model:

```go
const clipNamespace schema.Namespace = 200

def, err := index.VectorDef(vectoridx.Config{
    Namespace:   clipNamespace,
    Model:       "siglip2-base-patch16-224",
    Dimensions:  vectoridx.Dim768,
    ElementType: vectoridx.ElementFloat32,
    Metric:      vectoridx.MetricCosine,
}, "siglip2-base-patch16-224", schema.EntityImage)
if err != nil {
    return err
}
if err := defs.Register(def); err != nil {
    return err
}
```

Use a new namespace for a different checkpoint or embedding space. The
ImageID-to-namespace relationship is 1:1: each namespace stores one vector per
image for that model/space, and additional models or embedding slots use
additional namespaces. Do not mix SigLIP2 FixRes, SigLIP2 NaFlex, DINOv3, or
different model versions in one namespace. `vectoridx` currently supports
512/768/1024 dimensions, so a 768D SigLIP2 model fits; a 1536D model does not.

### 2. Put the image embedding in the database projection

The projection is the rebuild source for derived indexes, so the vector must be
there. The projection format is application-defined; version it if it can
change.

```go
func encodeProjection(id schema.ValueID, vec []float32) []byte {
    b := make([]byte, 8+4*len(vec))
    binary.BigEndian.PutUint64(b[:8], uint64(id))
    for i, v := range vec {
        binary.BigEndian.PutUint32(b[8+4*i:], math.Float32bits(v))
    }
    return b
}

func decodeProjection(p []byte) (index.Entry, error) {
    if len(p) != 8+768*4 {
        return index.Entry{}, database.ErrCorruptJournal
    }
    id := schema.ValueID(binary.BigEndian.Uint64(p[:8]))
    vec := make([]float32, 768)
    for i := range vec {
        vec[i] = math.Float32frombits(binary.BigEndian.Uint32(p[8+4*i:]))
    }
    return index.Entry{
        ValueID: id,
        Vectors: []index.VectorValue{{
            Namespace: clipNamespace,
            Element:   vectoridx.ElementFloat32,
            Float32:   vec,
        }},
    }, nil
}
```

At ingest:

```go
vec := make([]float32, embedder.Dim())
if err := embedder.EmbedImageIntoContext(ctx, vec, img); err != nil {
    return err
}

record := []byte("application record")
gen, err := db.Commit(database.Mutation{
    Entity:     schema.EntityImage,
    ValueID:    id,
    Record:     record,
    Projection: encodeProjection(id, vec),
})
if err != nil {
    return err
}
if err := db.WaitIndexed(ctx, gen); err != nil {
    return err
}
```

`db.Index()` is derived. Do not add vectors directly to it while the database
coordinator is running; write the projection and let the coordinator update the
vector index.

If the application also needs “more like this,” persist the embedding in the
canonical record or an application side table as well. `vectoridx` does not yet
expose a public “get stored vector by ImageID” API.

### 3. Query with the text embedding

Embed the query text with the **same checkpoint** used for image ingest, then
rank the plan:

```go
engine, err := search.New(db.Index())
if err != nil {
    return err
}

vec, ok := engine.Vector(clipNamespace)
if !ok {
    return search.ErrNamespaceUnavailable
}

// Liveness is added automatically. Add metadata/ACL clauses here as needed.
plan, err := engine.Compile(schema.EntityImage)
if err != nil {
    return err
}

query := make([]float32, embedder.Dim())
if err := embedder.EmbedTextIntoContext(ctx, query, text); err != nil {
    return err
}

hits, err := plan.RankVector(
    ctx, nil, search.NewWorkspace(256), vec, query, 50,
)
if err != nil {
    return err
}

for _, hit := range hits {
    // MetricCosine: Distance is 1-cos in [0,2].
    similarity := 1 - float64(hit.Distance)
    _ = similarity
}
```

`RankVector` applies plan liveness and ACL stages. A selective metadata plan
uses exact candidate scoring; a broad plan uses filtered ANN with over-fetch.

With metadata filters:

```go
clause, err := engine.Values(tagNamespace, bitmapidx.EnumStringValue("favorite"))
if err != nil {
    return err
}
filtered, err := engine.Compile(schema.EntityImage, clause)
if err != nil {
    return err
}
ws := search.NewWorkspace(256)
hits, err := filtered.RankVector(ctx, nil, ws, vec, query, 50)
```

### Model and scoring rules

- Use the same checkpoint for the image and text towers. Matching dimensions
  alone does not make two spaces compatible.
- SigLIP2 returns L2-normalized vectors. Use `MetricCosine` for simple score
  handling; `Distance` is `1-cos`. `MetricDot` is also valid for normalized
  vectors but its `Distance` is `-dot` and its score convention is different.
- Do not re-tokenize text with a generic tokenizer. Use the encoder’s text API;
  SigLIP2 handles lowercasing, EOS, truncation, and padding.
- Keep the vision preprocessing consistent between ingest and model export;
  SigLIP2’s `ReferenceResize: true` is the Hugging Face/Pillow-compatible path.
- The model/checkpoint ID is stored as `vectoridx.Config.Model` (maximum
  `vectoridx.MaxModelBytes`, 128 bytes) and is persisted in both the schema
  namespace config and the vector base header. Use a new namespace for a new
  model version rather than changing it in place.
- For a model upgrade, backfill a new namespace and switch reads; do not
  overwrite the old namespace in place.

### Text search variants

- **Text-to-image:** paired SigLIP2 text/image embeddings in one namespace.
  This is the path above.
- **Caption or document text search:** store text embeddings in a separate
  namespace using a text model, then map results back to `ImageID`. This is
  text-to-text, not cross-modal, unless the model is jointly trained.
- **Keyword search:** use bitmap text namespaces (`search.NamespaceText`) for
  tags/captions. Lexical and vector results can be combined with
  `search/ranking` (RRF), using `KindText` and `KindVector` lanes.

For a multi-million-photo library, cache text query embeddings by
`(model, normalized text)`. ONNX inference is much more expensive than the
vector search itself.

## Consistency and lifecycle

- `DB.Commit` durably writes canonical records and ordered journal entries; its
  returned generation is the last committed sequence.
- `DB.WaitIndexed(ctx, generation)` waits until derived indexes are durable and
  visible through that generation. Ordinary searches may lag the latest commit
  until this wait completes.
- `DB.Sync(ctx)` waits for every generation committed before the call.
- `DB.Checkpoint()` checkpoints derived indexes; canonical Pebble data is already
  durable. Run it during idle periods.
- `DB.Close()` stops new operations, drains the coordinator, closes the index,
  and releases locks. It may wait for active query plans to release their pinned
  snapshots; drain long-running reads before closing.

## Ownership and limits

- `DB.Get` returns caller-owned bytes.
- `Mutation.Record` and `Mutation.Projection` are caller-owned and may be reused
  after `Commit` returns.
- `search.Workspace` is exclusive to one execution and must not be shared or
  re-entered from a callback.
- `DB.Index()` is an advanced accessor. Do not write to or close it while the
  coordinator is running.
- `Options.BatchSize` caps one index-apply batch at 64 operations. A `Commit`
  call may contain more mutations, bounded by `MaxPending`, memory, and
  `MaxProjectionBytes`.
- `MaxProjectionBytes` defaults to 1 MiB; `OversizedRetentionBytes` defaults to
  4 MiB. Zero selects the documented defaults.
- `ValueID` zero is reserved. Entity-scoped IDs may overlap across entity kinds.
- Vector `ValueID`s must fit the vector engine's `uint32` NodeID range.

## Platform support

The repository CI exercises Linux `amd64` and Linux `arm64`. Build, full tests,
race tests, schema recovery, migration crash, and allocator checks run on both
platforms, including the internal SIMD-kernel equivalence suites. Native Apple
Silicon is used for development benchmarks, not as a CI replacement.

## Version policy

- `schema.dat` container/definitions semantics require a version bump and an
  explicit migration before changing.
- Canonical record versions and journal versions are fail-closed format
  contracts.
- Derived index files are versioned caches. They may be rebuilt or rejected
  when their format is unsupported.
- The documented 0.5 public API may still change before 1.0. Breaking changes
  require release notes and, for persistence, an explicit migration or
  fail-closed rejection. Vector APIs and formats remain experimental.

## Post-0.5

The following are intentionally deferred and are **not** 0.5 blockers unless a
demonstrated correctness or data-loss defect is found:

- per-family parallel writers and a composite root/epoch reclamation redesign;
- selective namespace/family recovery beyond full-index rebuild;
- adaptive batching and broad write-amplification work;
- vector batching, graph redesign, or vector API stabilization;
- new SIMD/SoA architecture or storage allocator changes;
- ranking redesign or new cardinality systems.

See [`AGENTS/`](AGENTS/README.md) for the current architecture and 0.5 contracts.
Historical design material is available in git history; source, tests, and
package documentation take precedence.
