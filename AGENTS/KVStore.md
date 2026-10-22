# KVStore — namespace-aware Pebble store

**Status:** current advanced-package guide. Authoritative package documentation:
[`store/README.md`](../store/README.md). This is not a proposal and does not
describe a MessagePack or versioned-key design.

## Contract

`store` owns one Pebble database and a set of explicitly registered `KindKV`
namespaces. It stores opaque application bytes; encoding, record versions, and
projections belong to the application or `schema`/`database`.

Every key is 12 bytes:

```text
namespace:uint16 BE | valueID:uint64 BE | flags:uint16 BE
```

- Namespace zero is reserved for the legacy schema key.
- `flags` is preserved in the key format; database-owned namespaces use zero.
- `Set` replaces the entire value. Partial value mutation is not supported.
- `Get` returns caller-owned bytes. `Scan`/`ScanFrom` values are borrowed only
  for the callback duration.
- `Batch` commits multiple namespaces atomically through Pebble's synced path.

## Open and schema ownership

```go
kv, err := store.Open(dir, definitions)
```

`Open` consumes the caller's authoritative `KindKV` definitions and validates
any legacy Pebble schema copy. Normal V1 operation never reads a runtime schema
registry from Pebble; `schema.dat` is authoritative. Adding KV namespaces is a
closed-store migration coordinated by `database`.

`OpenLegacy` and `OpenLegacyWithExtension` are migration-only. Do not use them
for a new V1 database.

## Relationship to database

`database` uses `store` for canonical values and the ordered journal. While the
database coordinator is running, an application must not open a second store on
the same directory or mutate reserved database namespaces. See
[`database/FORMAT.md`](../database/FORMAT.md).

## Verification

- `go test ./store/...`
- `go test -race ./store/...`
- Benchmark `Get`, `Set`, batch commits, and scans with reused destinations.

The old MessagePack/versioned-key proposal that previously occupied this file is
historical and is not part of V1.
