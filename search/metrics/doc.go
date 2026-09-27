// Package metrics hosts the shared rank-phase report used by search and
// search/ranking.
//
// Both packages fill the same RankMetrics fields with the same meanings so
// one report can travel across the native-score path and the uint16 fusion
// path without conversion:
//
//	Candidates    inputs examined (matches visited, signals encoded, IDs fused)
//	Scored        inputs that produced a usable score or rank
//	Skipped       inputs without usable data (missing lookup, NaN, NoRank)
//	ScoreDuration canonical lookups, encoding, fusion scoring, heap upkeep
//	SortDuration  final result ordering and Top-K selection sorts
//	K             requested top-k limit (TopK / rankByScore); RRF damping is
//	              configuration and never overwrites K
//
// Reports are caller-owned and reused sequentially, never shared between
// concurrent executions. A nil report selects the uninstrumented fast path;
// measured entry points reject nil reports. Durations accumulate with +=
// so one report can span an encode -> fuse -> sort pipeline.
//
// SearchOperation and StageMode also live here: the operation and stage
// vocabulary is shared so reports stay comparable across packages. The
// plan-bound machinery (SearchMetrics, stage records, reset/reach) stays in
// search because it reads unexported plan internals. Timer is the shared
// phase stopwatch backing ScoreDuration/SortDuration accumulation.
package metrics
