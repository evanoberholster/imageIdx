# imageIdx Agent Instructions

imageIdx is a performance-oriented Go image indexing and search library.

## V1 documentation

`database.DB` is the stable V1 application entry point. Start with
[`README.md`](README.md), [`database/README.md`](database/README.md), and
[`database/FORMAT.md`](database/FORMAT.md). Package documentation and tests are
authoritative; files under [`AGENTS/`](AGENTS/README.md) are architecture notes,
design history, or workflow guidance unless they explicitly say otherwise.

Vector APIs under `index/vectoridx` and vector portions of `index` are
experimental in V1.

## Repository navigation

Use SigMap as the default repository retrieval layer:

```sh
sigmap ask "<task or question>"
```

Start with the highest-ranked 2–5 files or symbols returned by SigMap. Read
more only when the implementation requires following a dependency. For a
topic-oriented lookup, use:

```sh
sigmap --query "<topic>"
```

Skip discovery when the user names the exact file and symbol and the change is
demonstrably local. Do not perform repository-wide grep, glob, or source reads
before discovery unless the initial SigMap result is insufficient.

## Engineering priorities

Prioritize, in order:

1. Correctness
2. Predictable low allocation
3. Query/read performance
4. Simple architecture
5. Storage efficiency

Prefer immutable reader-visible structures, lock-free readers, serialized
writes/publication, reusable scratch memory, zero-copy mmap reads, and SIMD
behind internal kernels. Avoid speculative architectural redesigns.

Keep package dependencies one-way: index packages may use shared `schema`,
`roar`, and `internal/storage` mechanisms; `search` may combine indexes, but
index packages must not import `search`. Namespaces are schema-defined: a
`schema.Definitions` holds unique namespace numbers and names, and every new
collection must register a `schema.NamespaceDef` (built from its
package-specific spec) before it is shared. `schema` owns entity identity,
identifier aliases (`ImageID`, `UserID`, `GroupID`), durability, and allocation.

## Implementation workflow

1. Locate the smallest relevant surface with SigMap.
2. Read the relevant implementation and tests.
3. Make the smallest correct change.
4. Run focused tests or benchmarks.
5. Run `sigmap validate` after source or configuration changes.

Use a new OpenCode session for unrelated features or index subsystems. Keep a
session for a feature, its direct fixes, and benchmark tuning.

Do not edit generated context files unless explicitly required; regenerate them
through SigMap instead.

## Validation

Run the smallest relevant checks first. Before handing off a broad change, run:

```sh
go fmt ./...
go build ./...
go test ./...
go test -race ./...
go vet ./...
sigmap validate
```
