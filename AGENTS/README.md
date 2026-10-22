# Agent documentation

Use the repository-root [AGENTS.md](../AGENTS.md) for agent workflow. These
documents are design notes, not competing implementations or authoritative API
specifications. For current behavior, prefer source, tests, and package docs.

## Current references

- [Build concepts](BuildConcepts.MD), [bitmap index](BitmapIndex.md), and
  [search](Search.md) are concise architecture guides.
- [`schema/README.md`](../schema/README.md) is the source of truth for schema
  definitions and entity ID allocation. `database.DB.NextID` uses this
  allocator; do not add a separate `index/idalloc` implementation.
- [`index/README.md`](../index/README.md) and
  [`search/README.md`](../search/README.md) document package contracts and
  examples.
- [Schema](Schema.md) and [KV store](KVStore.md) are additional subsystem
  notes; verify details against the current package documentation and code.

## Historical proposals

The remaining design documents record history and may not match current code.
In particular, [HashIdx2](HashIdx2.MD) and [CryptoHash](CryptoHash.MD) describe
retired or alternative hash implementations; use `index/radixhash` and
`index/simhash` instead. Check [Ranking](Ranking.MD), [Vector index](VectorIdx.MD),
[Radix tree hash](RadixTreeHash.MD), [SimHash](SimHash.md), [Time index](Timeidx.md),
[Roar format](RoarFormat.md), [bitmap kernels](BitmapKernel.MD), and
[benchmarks](Bench.MD) against current source before relying on them.
