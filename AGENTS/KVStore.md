# KVStore.md — Namespace-Aware Pebble Value Store

**Status:** Codex implementation specification · **Package:** `store` · **Dependency direction:** `common` → `store`; neither `index`, `schema`, nor `database` is imported by `store`.

## Objective and non-goals

Build a reusable KV engine over **one Pebble instance**, with shared `common.Namespace` definitions, fixed-width keys, atomic multi-namespace batches, safe zero-copy reads, ordered batched retrieval, and compact logical tombstones. It stores opaque bytes. Application record types, MessagePack generation, relationship indexes, and the indexing journal belong to other packages. Image binaries and thumbnails live in S3; no blobs or extra per-record compression belong here. Defer cache tuning.

## Shared namespace contract (coordinate with Schema.md)

- Preserve `common.Namespace uint16`, `common.ValueID uint64` and `common.EntityKind`.
- Use **one authoritative `common.Schema`** for KV and index namespace definitions. Every namespace number and name is unique, and definitions have a storage family/kind and owning entity. The physical KV store only opens `KindKV` definitions; `index.Store` opens index kinds. Neither silently accepts a kind assigned to the other.
- Support multiple KV namespaces owned by the same entity, e.g. image core, image EXIF, image AI analysis; the same ValueID can identify those records. IDs may overlap between different entity kinds.
- Register new namespaces **only while the database is closed**, via an explicit schema-extension/migration path. Existing namespace identity, kind, entity, and codec-relevant configuration are immutable; reject removal, renumbering, retyping and config mutation. Namespace 0 and internal/reserved namespace values must be validated consistently. Integrate with the *existing* `common.Schema` binary version rather than inventing an incompatible competing registry. Treat upgrade of historical on-disk schemas as an explicit migration, not a silent reinterpretation.
- Persist schema changes crash-safely before exposing newly created namespaces; add compatibility and interrupted-migration tests. Coordination of KV and index schema upgrades belongs to `database`, not `store`.

## Key encoding and on-disk value contract

```
key = namespace:uint16 BE | valueID:uint64 BE | schemaVersion:uint16 BE   // exactly 12 bytes
value = generated MessagePack bytes (NO version prefix)
```

- The 2-byte **record schema version is part of the key**, not the value. Use big-endian encoding for all key fields. The 10-byte `(namespace, valueID)` prefix groups versions of the same record and keeps namespace/ID ordering. `schemaVersion` is per record type, not a global database version.
- `schemaVersion=0` is reserved for the logical tombstone key; versions `1..65535` identify live payload formats. The tombstone value is empty. Do not confuse a tombstone with a zero-length live MessagePack payload.
- **One logically current version per `(namespace, valueID)`**: a full-record update (including a schema migration) must atomically delete any previous live version key(s) and write the new version key. A delete atomically removes live version key(s) and writes the version-0 tombstone key. A restore atomically removes the tombstone and writes one live version key. Avoid retaining historical versions as duplicate live records.
- `ValueID=0` is reserved for user records; internal metadata and journals use explicitly reserved namespaces/keys without colliding with user records. Existing `store.Key` encodings may use flags and different key sizes: **migration is mandatory**; do not reinterpret historical 10/11-byte keys as 12-byte keys. Coordinate format upgrades with `database` journal, tombstones, and recovery.
- Generic `store` must treat `schemaVersion` as an opaque `uint16` identifier and must never import `msgp` or deserialize payloads. Typed `schema` wrappers choose versions and perform migrations.
- Expose an efficient **exact-version** Get using all 12 key bytes, and a logical latest/current Get by the 10-byte prefix. Logical Get must distinguish no record, tombstone and a current live record; return both payload and actual schema version. A prefix lookup may require iterator/seek work; benchmark and make the performance tradeoff explicit rather than claiming equal cost to an exact Pebble.Get.
- For namespace iteration and ordered batch reads, group versions by the 10-byte prefix and return each logical record once. Reject conflicting multiple live versions or define an explicit repair policy; do not silently choose one and disguise corruption.
- A tombstone remains to preserve deletion state; it does not free the ValueID for reallocation. Journal indexing and recovery must correctly handle logical deletions and version migrations.

## API design

Keep the generic public API small and namespace-aware. Illustrative signatures (adapt to existing code; names are not mandatory):

```go
type Key struct { Namespace common.Namespace; ValueID common.ValueID; Version uint16 }
type Store struct { /* single Pebble database */ }
type Batch struct { /* Pebble batch */ }

func (s *Store) GetOwned(key Key, dst []byte) ([]byte, error) // exact version
func (s *Store) View(key Key, fn func([]byte) error) error
func (s *Store) GetBorrowed(key Key) (handle *ValueHandle, err error) // exact version
func (s *Store) NewBatch() *Batch
func (b *Batch) Put(key Key, value []byte) error
func (b *Batch) Tombstone(ns common.Namespace, id common.ValueID) error
func (b *Batch) Commit() error
func (b *Batch) Close() error
```

- Expose **both** a callback borrowing API and an explicit close/release borrowing API. Prefer a concrete `ValueHandle` with `Bytes()` and `Close()` over allocating a closure per lookup; signatures above are conceptual. Borrowed bytes are valid **only** until handle close/iterator advance. Never retain them in cached records or return them from a callback. Provide documented borrowed-value lifecycle tests, including on errors and store close.
- Add an explicit logical lookup returning the stored version, e.g. `GetCurrent(ns, id)` / `ViewCurrent(ns, id, fn(version, bytes))`. Version-specific lookup is always a separate API; do not accidentally expose a stale historical record as current.
- `GetOwned` may append to caller-controlled `dst` (avoid allocations when capacity suffices). `Get` compatibility with the current package should be preserved or migrated deliberately.
- Batch operations span any declared KV namespaces and atomically commit once. Validate all namespace/ID/size constraints before publication. Expose explicit sync vs async Pebble durability modes when needed, but `database`'s canonical+journal commit must use a **durable sync commit**.
- Preserve input order in batch reads, including duplicate IDs. Report not-found/tombstoned status **per input**, not by reordering/dropping entries. Provide caller-owned destinations/workspaces and a callback or owned alternative; never return borrowed slices after their handles close.
- Do not claim one Pebble iterator is equivalent to arbitrary-ID point gets. Benchmark native point gets versus sorted multi-seek/iteration; sort only scratch indices (not caller input) when beneficial, then scatter results back to original order.
- Namespace iteration should support lower/upper key bounds and safe borrowed values. Validate iterator lifetime, corruption handling and caller cancellation at the coordinator layer. Use a Pebble snapshot when a consistent scan is required; make iterator ownership explicit.
- Normal reads must distinguish missing, tombstone, and I/O/corruption errors; never silently turn corruption into not-found.

## Namespace schema evolution and database integration

- Additive namespace registration takes place on **closed-store reopen** with an explicit, durable schema upgrade operation. Old namespaces remain byte-for-byte semantically equivalent. The `database` layer must coordinate schema change across Pebble and index manifests: there is no assumption that replacing one schema file atomically upgrades both systems.
- The generic `store` exposes the primitives for `database` to atomically commit canonical record changes plus journal entries. Journal reservation keys and generation metadata must be allocated via a documented internal key scheme disjoint from user KV keys.
- `store` does not update secondary indexes, does not depend on `schema`, and does not implement cross-index transactions.

## Performance rules

- Avoid maps, reflection, per-value interface dispatch, and per-record goroutines on hot paths. Favor `[12]byte` stack keys, reusable batches, per-call scratch, and borrowed buffers. Do not add a new buffer pool unless benchmarks justify it.
- Zero-allocation *steady-state* means with adequate caller-owned buffers and generated codecs, not the first open, Pebble internal allocations, or newly retained result objects. Benchmark `AllocsPerRun`; document any unavoidable Pebble allocations.
- Full-record replacement only; no partial MessagePack mutation. No per-record Zstd; rely on Pebble’s normal storage configuration.

## Required tests and benchmarks

1. 12-byte lexicographic ordering, BE round-trips, version ordering within identical namespace/ID, max values, rejection of reserved versions/IDs/namespaces, namespace isolation and overlapping ValueIDs.
2. Atomic multi-namespace batch commits, aborted batch, concurrent access, crash/reopen, and sync-commit durability.
3. Borrowed value handle/iterator lifetime, callback error paths, owned copies, no data alias after release, Close behavior, `-race`.
4. Version-0 tombstone put/delete/reopen, idempotent deletion, explicit recreation, and distinction between missing, tombstone, and live MessagePack. Test atomic deletion of old version keys.
5. Ordered batch gets with duplicates, missing/tombstoned IDs, ascending/descending/random input, 1/16/64/128 keys.
6. Additive namespace schema upgrade on reopen; reject illegal schema changes and interrupted upgrades.
7. Benchmarks: `GetBorrowed`, `View`, `GetOwned` with reuse, 64-result ordered batch reads, 64-operation atomic Put, namespace scans, tombstones; compare against base branch via `benchstat`.

**Merge gate:** `go build ./...`, `go test ./...`, `go test -race ./store/... ./common/...`, `go vet ./...`, crash-reopen tests and actual benchmark measurements. Keep existing public API compatible or document explicit migration, never silently break persisted formats.
