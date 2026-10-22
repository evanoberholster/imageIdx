## Phase 3.1: serialized write-path gate

This PR documents and applies the Phase 3 alternative gate for writer publication.

### Writer protocol decision

The required per-family staged-generation protocol cannot be made safe in Phase 3 without a broad engine/query redesign:

- each engine publishes its own atomic view;
- queries load raw engine handles independently;
- there is no composite root or query-lifetime snapshot pin;
- independent family publication would expose mixed generations.

The full decision is recorded in `AGENTS/Refactor/Phase3-Writer-Protocol.md`. Phase 3.1 keeps the single serialized index apply writer and does **not** claim a family-writer throughput win. The composite root/epoch design remains required before family workers or coherent query snapshots.

### Safe serialized-path changes

- `timeidx.Batch.Append` now collapses repeated ID-authoritative mutations in the writer buffer, preserving last-write-wins while reducing persistence work.
- Canonical commits skip an identical canonical value rewrite.
- Projection diffing is skipped when the durable projection bytes are unchanged.
- Added regression tests for last-write-wins time batches and identical canonical commits.

### Measured effect

For the `unchanged` batch-64 workload, `store-B/op` dropped from **70.5** to **27.5** (about 61% fewer logical store bytes). This is a targeted no-op write reduction, not the 15% family-writer throughput target.

### Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate` — 90% indexed-file coverage

### Explicit limitations

- No per-family writer goroutines.
- No new composite read root/epoch reclamation.
- No family-writer throughput claim.
- The 15% acceptance target remains unmet; this PR records the failing proof and applies the approved fallback.
