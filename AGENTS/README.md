# AGENTS

Project-specific agent resources.

- [BuildConcepts.MD](BuildConcepts.MD) — how the project is organized, built,
  tested, and extended. Read this first.
- [BitmapIndex.md](BitmapIndex.md) — implemented `bitmapidx` design and format:
  persistent typed fields (enum, bool, text, continuous, geo).
- [Timeidx.md](Timeidx.md) — implemented `timeidx` design: exact 1900-epoch
  nanoseconds, namespace-aware copy-on-write shards, and the source-completeness
  watermark. Package detail lives in `index/timeidx/README.md` and
  `index/timeidx/BENCHMARKS.md`.
- [SimHash.md](SimHash.md) — implemented `simhash` design: exact
  `pHash64 + PDQ256 + quality` similarity store, mmap columnar format, batch redo
  WAL.
- [Search.md](Search.md) — combined query planning and execution.
- [RoarFormat.md](RoarFormat.md) — standalone zero-copy roaring format.
