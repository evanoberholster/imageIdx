# IDAllocator.md — Lock-Free Monotonic Entity ID Allocation

> Historical specification for the retired Pebble-backed `index/idalloc`
> implementation. The active namespace allocator is `schema/` (see
> `schema/README.md`). `database.DB.NextID` now uses that package with one
> canonical namespace per entity. Do not recreate this retired package.

**Status:** Codex implementation specification · **Suggested package:** `index/idalloc` (or a small subpackage of `index` without engine dependencies).

## Objective

Provide one process-local allocator service with **independent increasing uint64 ID sequences per `common.EntityKind`**, including User, Image, Location, Person, Face, Album, Stack, Tag and future entity kinds. A valid ID is nonzero; entity IDs may numerically overlap. Reserve **1,000 IDs per entity** at a time using a durable reservation store; the normal `Next` path must avoid locks, heap allocations and storage I/O. **Gaps are explicitly permitted** (allocation without record commit, unused reservation after crash, cancellation, deletion). Never reuse an ID that may have been handed out.

## Critical correctness rule

**Do not recover the allocator by scanning only live KV records.** An issued ID might be uncommitted, deleted, or only held in memory when the process crashes. The highest surviving record cannot prove the highest issued ID. The authoritative source for restarting is the **durably persisted reservation high-water mark**. A canonical KV scan is useful only for migration/reconciliation: if a record's ID exceeds the persisted high-water mark, reserve beyond it before issuing more IDs. This is required despite the original proposal to find the last ID after a crash. Reservation high-water is the minimal robust way to guarantee non-reuse.

## Data and persistence

- Keep allocator logic independent of Pebble, via a small storage interface that atomically and durably reads/advances a reservation high-water mark per entity kind. Implement a Pebble adapter in `store` or database wiring, not by importing `store` into core index packages.
- Durable metadata keyspace must be disjoint from user `[namespace:uint16][ValueID:uint64][schemaVersion:uint16]` 12-byte record keys and the indexing journal. Metadata contains format version, entity kind, `reservedThrough:uint64`, and optionally checksum if not already protected by Pebble framing.
- On first use, reserve `[1, 1000]` (subject to overflow). Before publishing an ID from a new reservation, commit the new high-water mark with Pebble sync durability. A failed reservation publishes **zero** IDs from that block.
- Clean Close may persist a diagnostic `lastIssued` value but **must not lower** the durable reservation high-water mark, reclaim IDs, or make previously issued IDs reusable. On reopening, resume above the durable high-water mark. This intentionally leaves gaps.
- Monotonic means unique and ordered by each allocator's atomic ticket order, **not** by concurrent database commit order. Do not promise globally gap-free sequences.
- Reject overflow; never wrap to 0.

## Concurrency design

- One allocator service per open database, with one predeclared slot per entity (dense indexing by `EntityKind`; no maps/reflection on the allocation path). Exactly **one process** owns this database/allocator. Cross-process allocation is unsupported and must fail via existing database ownership/lock rules.
- `Next(kind)` uses an atomic compare-and-swap on the current next/ticket, bounded by an atomically published reserved limit. A fast path must not involve a mutex or Pebble call. Test with hundreds of goroutines crossing block boundaries.
- Slow-path reservation can serialize with a mutex or atomic state machine; lock-free hot path does not require a lock-free durable reservation. Precisely one reservation operation may publish a given new block. Concurrent allocators must not produce duplicates when one goroutine pauses at the boundary.
- Concurrency must not expose a new limit before the durable high-water write succeeds. Make close coordination explicit: `Close` rejects new allocations and waits for in-flight reservations; an allocator may not issue IDs after its backing store closes.
- Batch allocation can reserve an in-memory span using CAS without extra allocations, but beware ranges that straddle a reservation boundary. Keep API simple unless benchmarks warrant additional methods.

## Example conceptual API

```go
type Allocator struct { /* fixed entity slots */ }

type HighWaterStore interface {
    LoadReservedThrough(entity common.EntityKind) (uint64, error)
    ReserveThrough(entity common.EntityKind, newHigh uint64) error // synced, monotone
}

func Open(reservations HighWaterStore) (*Allocator, error)
func (a *Allocator) Next(entity common.EntityKind) (common.ValueID, error)
func (a *Allocator) Close() error
```

The interface is illustrative: prefer existing conventions and ensure `ReserveThrough` is idempotent and rejects lowering the watermark. Explicitly document whether a `Next` failure consumes an ID. Avoid hiding I/O on the common path.

## Integrating with shared namespaces

- **Allocator slots are keyed by entity type, not by individual KV or index namespace.** `ImageInfo`, `ImageEXIF`, and `ImageAI` share the same `ImageID`; a single entity value is referenced by many namespaces. `PersonID(10)` and `ImageID(10)` are distinct identities.
- Namespace definitions come from `common.Schema`, including entity ownership. New entity kinds require an explicit schema/allocator version evolution; new namespaces for existing entities do not create another allocator counter.
- IDs are issued before canonical record commit when callers need them, and can be abandoned. The `database` coordinator still commits records and index journal atomically; allocating an ID is **not** itself a canonical record transaction.
- Deletion and tombstoning never return IDs to the allocator. A new record must not be created with an older ID unless an explicitly authorized restore operation has been designed outside the allocator.

## Crash safety and startup

1. Open Pebble and enforce its exclusive writer ownership.
2. Read each entity's durable `reservedThrough` value.
3. For legacy stores without allocator metadata, execute an **explicit one-time migration** scanning all relevant canonical and tombstone records, determine max IDs per entity, durably advance high-water marks, then allow allocation. Do not scan full KV on every normal startup.
4. Set next issueable ID to `reservedThrough + 1` and reserve the next block before issuing.
5. If reservation metadata is corrupt or missing from an already initialized database, **fail closed** instead of silently resetting counters.

## Tests and benchmarks

- Sequence per entity; two different kinds may return the same number; namespaces owned by the same entity share one counter.
- 1,000-ID exact boundary, partial block, crash after reservation / before issuance, crash after issuance / before canonical commit, clean close, reopening, and ID non-reuse with tombstones.
- 32+ concurrent goroutines with randomized pauses and repeated reservations; collect and sort IDs per entity and prove uniqueness. Concurrent commit order is not asserted to be increasing.
- Inject persistence failures before/during reservation; no ID from uncommitted block escapes. Verify high-water monotonicity and overflow near `math.MaxUint64`.
- Migration from existing datasets containing sparse, high, deleted/tombstoned IDs.
- `Next` steady-state `AllocsPerRun == 0` with no I/O; throughput (1/8/32 goroutines), reservation latency, contention and memory usage. Compare with `main` where applicable.

**Merge gate:** `go build ./...`, `go test ./...`, `go test -race ./index/idalloc/...` (adjust actual path), crash-subprocess tests, and reproducible `benchstat`. Do not claim gaps are prohibited, and do not optimize slow-path reservation at the expense of non-reuse safety.
