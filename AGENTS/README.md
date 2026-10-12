# Agent documentation

Use the repository-root [AGENTS.md](../AGENTS.md) for workflow. The concise,
current architecture notes are [BuildConcepts.MD](BuildConcepts.MD),
[BitmapIndex.md](BitmapIndex.md), and [Search.md](Search.md). Package
`index/README.md` and `search/README.md` contain public contracts and examples;
source and tests are authoritative for API details.

Other files here are **historical design notes/proposals**, not instructions or
verified API references. In particular `HashIdx2.MD` and `CryptoHash.MD`
describe retired/alternative hash implementations: use `index/radixhash` and
`index/simhash` instead. `Ranking.MD`, `VectorIdx.MD`, `RadixTreeHash.MD`,
`SimHash.md`, `Timeidx.md`, `RoarFormat.md`, `BitmapKernel.MD`, and `Bench.MD`
may be useful for design history but should be checked against current code.
