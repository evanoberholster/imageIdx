// Package search compiles conjunctive queries over bitmap namespaces, S2 cells,
// timestamps, and optional ACL grants into immutable, reusable Plans.
//
// # Compilation and snapshots
//
// Engine.Compile binds a plan to a unified index.Store and automatically adds
// the entity liveness restriction. Plans compiled by an Engine capture one
// reader-pinned engine snapshot at the start of each execution, so all clauses
// and result assembly observe one coherent publication generation while writers
// continue publishing newer generations.
//
// Direct Compile and CompileWithStore plans are lower-level: they hold live
// engine handles and must keep their indexes open. A single direct-plan
// execution may observe different published views in different stages.
// SearchOnce is the single-use direct-plan shortcut.
//
// Snapshot pins are held through the whole execution, including Visit
// callbacks. Engine reclamation may wait for an old pinned mapping before
// unmapping it; this is storage-retirement backpressure, not a query read
// lock. Successive pages or separate executions may observe later writes.
//
// # Execution
//
// AppendIDs collects sorted unique IDs; AppendPage seeks after an ID and stops
// after the requested number of verified matches. FilterIDs preserves candidate
// order and duplicates. Count avoids a result-ID slice; Exists stops at the
// first match. Visit reports cancellation and early callback stop.
//
// Workspaces are exclusive, reusable scratch. Executor pools them across plans
// with a per-workspace retained-memory budget. Index deltas, mmap detachment,
// snapshot handles, and buffered timestamp fallbacks can still allocate; a
// workspace is not a universal zero-allocation guarantee.
//
// RankTime preserves uint64 timestamp gaps; RankGeo preserves float64 metres.
// FilterRanked probes supplied candidates and preserves integer scores and input
// order. Top-k uses native scores, never saturated uint16 values. Optional score
// quantization belongs after ranking, at the presentation boundary.
//
// AllValues, NamespaceText, NamespaceRange, During, and InCells defer validation
// to Compile. Cached estimates only select strategies; Refresh explicitly
// rebuilds their order without repeating canonicalization or tokenization.
// Timestamp writer batches remain owned by timeidx callers.
package search
