//go:build race

package vectoridx

// raceEnabled reports whether the race detector is active. Allocation
// assertions are calibrated for production builds; race instrumentation
// adds its own bookkeeping allocations.
const raceEnabled = true
