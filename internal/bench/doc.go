// Package bench provides the shared benchmarking harness used by the index
// packages: deterministic datasets, precomputed workload distributions,
// latency recorders, open- and closed-loop runners, and memory/disk
// reporting.
//
// # Determinism
//
// Every generator takes a fixed seed and every benchmark name derives only
// from stable labels, so results are reproducible and benchstat-comparable:
//
//	go test -run='^$' -bench=. -benchmem -count=10 ./...
//
// # Size tiers
//
// Sizes reports the standard dataset sizes. The 10M tier is explicit: it is
// included only when IMAGEIDX_BENCH_LARGE=1, and tiers above 100K are
// dropped under testing.Short so ordinary test runs stay cheap.
//
// # Separation
//
// Microbenchmarks measure one operation with setup outside the timed region.
// Latency/distribution workloads use Recorder plus OpenLoop or ClosedLoop
// and are kept in separate benchmarks; recording overhead never runs inside
// a microbenchmark loop.
//
// This package deliberately returns only plain data (IDs, timestamps, hash
// words) and never imports an index package, so same-package tests can use
// it without an import cycle.
package bench
