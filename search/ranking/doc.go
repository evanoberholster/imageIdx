// Package ranking provides small, high-performance uint16 ranking signals,
// RRF fusion, deterministic sorting and Top-K selection over candidate
// ImageIDs.
//
// # Invariants (documented in AGENTS/ARCHITECTURE.md)
//   - Higher Rank is better. NoRank (0) means missing/unranked and sorts last.
//     Ordinary values are 1 (worst) .. 65535 (best). Exact matches encode to
//     65535. This inverts the proposal's lower-is-better convention so that a
//     zero value cleanly represents "no rank" without a separate validity
//     bitmap and without conflating missing data with an exact match.
//   - Each ranker is monotonic within its own signal only. Values from
//     different signals are not comparable; fusion is responsible for
//     combining heterogeneous signals.
//   - Signals use Structure of Arrays: one ImageIDs slice plus up to four
//     uint16 lanes. Lanes are execution lanes, not fixed meanings. Positional
//     correspondence ImageIDs[i] <-> Values[s][i] is preserved until after
//     fusion; only final results are reordered.
//   - All hot paths take caller-owned destinations and perform zero
//     allocations once buffers have capacity. No interfaces, maps, or
//     reflection appear in per-image loops.
//   - Final ordering is deterministic: higher fused Rank first, ties broken
//     by ascending ImageID.
//   - Rank-phase metrics use the shared search/metrics report with the same
//     field meanings as search.SearchMetrics.Rank: Candidates/Scored/Skipped
//     counts plus ScoreDuration (encode and fusion) and SortDuration (order
//     and Top-K). Nil reports select the fast path; WithMetrics entry points
//     reject nil reports.
//

// Bulk integer/float encoding goes through internal/rankkernel behind a
// scalar dispatch seam so future AVX2/NEON backends can replace the bulk
// loops without changing the public API. Only the scalar backend ships now.
package ranking
