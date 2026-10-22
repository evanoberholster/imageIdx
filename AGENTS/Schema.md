# Schema.md — Versioned MessagePack Entities and Zero-Allocation Index Projections

**Status:** Codex implementation specification · **Package:** `schema` (one package initially) · **Generator:** `github.com/tinylib/msgp`.

## Objective

Define typed application entity records, versioned MessagePack codecs, allocation-conscious typed KV wrappers, and projections to ImageIdx indexes. Schema evolves as the photo application grows. `store` remains generic, `index` remains generic, `database` coordinates durable canonical+journal+index updates. Actual photo files and thumbnails are in S3; schema holds references/metadata, not the binary assets. No record-level compression.

## Entities and extensibility

Initial typed families: `UserInfo`, `ImageInfo`, `LocationInfo`, `PersonInfo`, `FaceInfo`, `AlbumInfo`, `StackInfo`, `TagInfo`; additional entities as needed. Define aliases and entity ownership using existing `common` types; do not invent incompatible ID types. Support multiple KV record namespaces for one entity, such as `ImageInfo`, `ImageEXIF`, `ImageAI`, so frequent analysis updates do not rewrite unrelated EXIF fields.

Keep `schema` as one package, organized by files (`image.go`, `face.go`, `person.go`, etc.). Generated `msgp` files are committed alongside the types. Use `go:generate` with pinned tool version and deterministic generation checks. **Do not use reflection-based MessagePack encoding in hot paths.** Generated codec methods should be benchmarked and tested; no claim of universal zero allocations when decoding strings, variable-length slices or retaining records.

## Shared namespace registry

- All storage and index namespace IDs are `common.Namespace uint16`, registered in one shared `common.Schema`, with unique IDs, unique names, family/kind and owning `common.EntityKind`.
- KV namespaces and index namespaces are distinct **definitions** even when they refer to the same entity/ValueID. E.g. `ImageEXIF` is a KV namespace, `ImageISO` a bitmap index namespace. A single `ImageID` can occur in many namespaces.
- Additional namespaces may be registered **only after all database/index stores close**, followed by coordinated, crash-safe schema extension on reopen; existing definitions are immutable. Old records are not rewritten simply because a namespace is added. Define a migration plan for any current persisted schema that rejects extensions.
- Keep typed constants and code-generated wrapper configuration in sync with `common.Schema` definitions; fail startup if an expected kind/entity mapping is inconsistent. Reserve internal metadata and journal namespaces.

## Record encoding and versioned 12-byte keys

```
key   = namespace:uint16 BE | valueID:uint64 BE | schemaVersion:uint16 BE
value = generated msgp payload  // NO version bytes in value
```

- The record schema version lives **only in the key** (last two bytes); values contain raw generated MessagePack. All key components are big-endian, and keys are exactly 12 bytes. This replaces the prior `[version:uint16][msgpack]` value envelope.
- Version is **per record type**; start at 1. Version 0 is reserved by `store` for tombstones and must never be decoded as a live record.
- Typed Get first resolves the logical `(namespace, ID)` to its current version, then dispatches the payload to the matching generated version decoder. An exact-version API is available when the caller already knows the version. Unknown future versions return `ErrUnsupportedSchemaVersion`; malformed MessagePack returns a distinct error.
- Schema upgrades use one atomic Pebble batch: remove the old live version key, insert the new `(namespace, ID, version)` key with its new payload, and remove a tombstone if restoring. There must not be multiple current versions visible after a commit.
- Keep historical decoder structs for supported versions and up-convert into the current Go type. Re-encode lazily on the next successful full-record update. Existing schema-specific indexes need rebuilding only if projection semantics change, not just because the key version changes.
- `schema` knows MessagePack and version dispatch; generic `store` knows only the key's version number and the borrowed payload bytes. No extra per-record value header, and no record-level compression.

## Typed, allocation-conscious wrappers

Provide generated, typed helpers with concrete methods and no reflection, for example (conceptual; adapt to actual store API):

```go
type Images struct { KV *store.Store; Namespace common.Namespace }
func (s Images) GetInto(id common.ImageID, dst *ImageInfo, scratch []byte) error
func (s Images) View(id common.ImageID, visit func(*ImageInfo) error) error
func (s Images) AppendEncoded(dst []byte, record *ImageInfo) ([]byte, error)
func (s Images) Put(batch *store.Batch, id common.ImageID, record *ImageInfo, scratch []byte) error
```

- ValueIDs are stored only in the key; the payload may reference related IDs but need not redundantly embed its own ID. Validate record/key consistency when a record does contain its ID.
- `GetInto` should decode into a reusable, caller-owned struct. Clear stale fields before decoding where generated code does not guarantee overwrite. Preserve lifetime safety: decoded strings/byte slices must not retain borrowed Pebble memory beyond its release unless the type/API explicitly exposes a borrowed view with an enforced lifetime.
- Borrowed API is for **encoded** bytes, or lifetime-bound decoded views with no retention. Do not claim a normal decoded Go string or slice is always zero-copy: decoder behavior and ownership must be measured. Provide owned-copy variants for retained records.
- Ordered batch reads preserve input order, including duplicates and missing IDs; reuse typed destinations and scratch slices. Do not allocate a per-item interface/closure on the hot path where concrete loop methods suffice.
- Typed wrappers perform no independent Pebble commit when used inside an application transaction: accept a `*store.Batch`, so multiple entity records and journal metadata can be committed atomically by `database`.

## Relationships

- Do **not** embed large `[]ImageID` or `[]FaceID` membership lists as the authoritative relationship representation in MessagePack.
- Store membership in dedicated indexes, primarily `bitmapidx`, with set semantics (no duplicate membership). Only maintain reverse membership indexes when query requirements justify them; bidirectionality is **not** automatic.
- E.g. `AlbumInfo` stores title/settings; album→images is an index. `PersonInfo` stores identity/display metadata; person→faces is an index. `FaceInfo` may store one PersonID reference if semantically useful, but do not create a duplicate secondary index without need.
- Ordering of an album/stack is **not** represented by a set-valued bitmap. If ordering is required, define a separate ordered relation or explicit position metadata, then index it intentionally.

## Schema-owned index projections

Minimize handwritten glue: each typed record provides methods that append the index associations it owns into caller-provided, reusable buffers. The `schema` package may import `index`, and `database` may import both `schema` and `index`; **neither `index` nor `store` may import `schema`**. Avoid cycles.

Suggested concrete method (illustrative):

```go
func (r *ImageEXIF) AppendIndexEntry(dst *index.Entry, ns IndexNamespaces) error
```

- Use generated or code-generated projection methods when beneficial; static code with typed namespace constants is preferred over maps/reflection or a generic reflective field-tag walker.
- **No heap allocations with adequately preallocated destination slices on the steady-state projection path**, where feasible. Callers own scratch and lifetime. Benchmarks must verify this explicitly; encoding variable-length tokens may necessitate owned bytes and should be reported honestly.
- Each KV record namespace projects **independently** into its corresponding set of index namespaces. A change to `ImageAI` must not require fetching or re-indexing unrelated `ImageEXIF` or `ImageInfo` unless cross-record derived fields explicitly require it.
- The `database` layer merges before/after projections into canonical `index.Entry`/`index.Change` writes and maintains durable journal ordering. For updates/removals, preserve previous projections or equivalent authoritative old state so obsolete hashes/tags/timestamps cannot survive. No secondary index writes on a plain `store` Put outside the coordinator.
- **Avoid destructive liveness transitions**: deleting `ImageAI` must not delete the whole Image entity, and a field update touching no indexed values must not accidentally remove entity liveness. Define a primary/core record that determines entity existence. Treat deletion of an entire entity separately from deletion of one auxiliary record namespace.
- Design for same numeric ValueID across entity kinds; never leak image ACL grants into Face/Person results.
- Current `simhash` dense-ID and vector node-ID limits remain explicit validation constraints; do not silently remap external IDs as part of schema generation.

## Version migration API

For each record type, provide version dispatch and conversion functions, e.g. `DecodeImageInfo(version, payload, into)` and `UpgradeImageInfoV1(v1, intoCurrent)`. Keep old decode structures only for versions still supported. Tests must cover additive fields, renamed fields, incompatible type changes, unknown future versions, malformed/truncated 12-byte keys, malformed MessagePack and deterministic re-encoding.

Use stable schema IDs/field names and stable namespace numbers. Treat index schema migrations separately from MessagePack record version changes; an old MessagePack payload can decode successfully even when an index requires rebuilding because the indexing projection changed.

## Deliverables

1. Initial entity structs and namespace definitions, plus generated `msgp` codec files.
2. Shared 12-byte key/version helpers and typed versioned decode/upgrade dispatch.
3. Concrete typed wrappers for `store`, including borrowed/owned read paths and batch puts.
4. Static index projection methods with caller-owned scratch.
5. Tests establishing correct ownership, zero-alloc steady-state paths, and before/after index invalidation.
6. `go:generate` instructions and a check that generated files are current.
7. A migration/versioning guide describing both record versions and namespace addition on reopen.

## Required verification

- Golden byte fixtures for 12-byte `(namespace, ValueID, version)` keys and raw MessagePack payloads, cross-version migration and malformed/corrupt value cases.
- Cross-namespace retrieval of the same ImageID; different entity kinds with identical numeric ValueIDs.
- Full-record replace, transaction batch, tombstone, record-specific auxiliary deletion, concurrent readers.
- Model queries using bitmap membership for Album/Tag/Person with no duplicate members.
- `AllocsPerRun` and `-benchmem` for generated encode/decode, `AppendIndexEntry`, typed single get/put and 64-result batch lookup; distinguish borrowed encoded bytes from decoded owned structs.
- Run `go generate ./schema/...`, `go build ./...`, `go test ./...`, `go test -race ./schema/... ./store/... ./database/...`, `go vet ./...` and benchmark comparisons. Do not mark done on unit tests alone; verify end-to-end canonical → projection → journal → index → search consistency.

**Scope guard:** Keep app-specific schema in `schema`, KV mechanics in `store`, ID reservations in `index/idalloc`, and durable orchestration in `database`. Do not add reflective generic object stores, dynamic runtime namespaces, new SIMD, general-purpose reverse relationship indexes, or S3 clients to these PRs.
