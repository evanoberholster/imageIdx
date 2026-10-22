# store — namespace-aware Pebble KV

`store` is the advanced, stable KV primitive used by `database` for canonical
records and the mutation journal. It is also available for standalone Pebble
collections. Applications that need schema, indexing, or recovery coordination
should use [`database.DB`](../database/README.md).

## Key contract

Every key is exactly 12 bytes:

```text
namespace:uint16 BE | valueID:uint64 BE | flags:uint16 BE
```

- Namespace zero is reserved for the legacy persisted schema key.
- `ValueID` zero is reserved by the higher-level database contracts.
- `flags` is preserved in the key format but is not used by the database-owned
  namespaces. Database-owned keys use `flags = 0`.
- `Set` replaces the full value; partial value mutation is not supported.
- `Get` returns caller-owned bytes. `Scan` values are borrowed only for the
  duration of the visit callback.

## Open and schema ownership

```go
defs := schema.NewDefinitions()
defs.Register(schema.NamespaceDef{
    Namespace: 200,
    Name:      "canonical-metadata",
    Kind:      schema.KindKV,
    Entity:    schema.EntityImage,
})

kv, err := store.Open("./data", defs)
if err != nil { return err }
defer kv.Close()
```

`Open` consumes the caller's authoritative `KindKV` definitions and never reads
or writes a serialized runtime schema in normal operation. It validates that the
directory's persisted legacy schema copy, when present, is compatible. Adding
KV namespaces is an explicit closed-store migration coordinated by `database`;
it is not supported at runtime.

`OpenLegacy` and `OpenLegacyWithExtension` exist only for migration fixtures and
pre-`schema.dat` construction. Do not use them with a database-managed V1 store.

## Mutations

```go
key := store.Key{Namespace: 200, ID: 7}
if err := kv.Set(key, value); err != nil { return err }

batch := kv.NewBatch()
defer batch.Close()
if err := batch.Set(key, first); err != nil { return err }
if err := batch.Set(key, second); err != nil { return err }
if err := batch.Commit(); err != nil { return err }
```

`Store.Set`, `Delete`, and `Batch.Commit` use Pebble's synced durability path.
A batch is atomic across all registered KV namespaces. Values are copied before
the call returns; callers may reuse their buffers.

## Reads and ownership

- `Get` returns an owned copy; missing keys return `ErrNotFound`.
- `Scan`/`ScanFrom` visit keys in namespace and ID order; the value slice is
  valid only until the callback returns.
- `Stats` is a best-effort application-level counter, not a device I/O or
  fsync report.
- `Close` releases the Pebble handle and lock. Do not use a batch or scanned
  value after the store is closed.

## Errors

| Error | Meaning |
|---|---|
| `ErrNotFound` | Key absent (wrapped Pebble not-found) |
| `ErrNamespace` | Namespace not registered for this store |
| `ErrSchemaMismatch` | Persisted legacy schema differs from requested definitions |
| `ErrEmptySpecs` | No KV definitions supplied |
| `ErrInvalidDir` | Empty store directory |
| `ErrInvalidKey` | Encoded key is not 12 bytes |

## Relationship to database

`database` owns the canonical and journal namespaces and must be the only writer
while its coordinator is running. Do not open a second `store.Store` on a
database directory or mutate reserved database namespaces through a standalone
handle. See [`database/FORMAT.md`](../database/FORMAT.md) for the key and value
layout.
