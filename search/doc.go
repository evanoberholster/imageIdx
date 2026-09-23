// Package search compiles conjunctive queries over bitmap fields, S2 cells,
// and timestamps into immutable, reusable Plans.
//
// AppendIDs collects sorted unique IDs; AppendPage seeks after an ID and stops
// after the requested number of verified matches. FilterIDs preserves candidate
// order and duplicates. Count avoids a result-ID slice; Exists stops at
// the first match. Visit reports cancellation and early callback stop.
//
// Workspaces are exclusive, reusable scratch. Executor pools them across plans
// with a per-workspace retained-memory budget. Index deltas, mmap detachment,
// and buffered timestamp fallbacks can still allocate; a workspace is not a
// universal zero-allocation guarantee. Bitmap sources use detached snapshots
// for cursors, so user callbacks do not hold mmap reader guards.
//
// Each index preserves its existing publication model. There is no global
// snapshot: stages, candidate batches and successive pages can observe different
// published views. Cached estimates only select strategies; Refresh explicitly
// rebuilds their order without repeating canonicalization or tokenization.
//
// RankTime preserves uint64 timestamp gaps; RankGeo preserves float64 metres.
// FilterRanked probes supplied candidates and preserves integer scores and input
// order. Top-k is selected using native scores, never saturated uint16 values.
// Optional score quantization belongs after ranking, at the presentation boundary.
//
// AllValues, FieldText, FieldRange, During, and InCells defer validation to
// Compile. SearchOnce compiles and executes once; Executor pools workspaces
// across plans. Timestamp writer batches remain owned by timeidx callers.
package search
