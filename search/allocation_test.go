//go:build !race

package search

import (
	"context"
	"testing"
)

// Pool-backed query scratch may be discarded deliberately by the race
// detector, so allocation assertions belong only in ordinary builds.
func TestCountNoAlloc(t *testing.T) {
	plan := broadPlan(t, 100_000)
	ws := NewWorkspace(256)
	if count, err := plan.Count(context.Background(), ws); err != nil || count != 100_000 {
		t.Fatal(count, err)
	}
	if allocs := testing.AllocsPerRun(10, func() { _, _ = plan.Count(context.Background(), ws) }); allocs != 0 {
		t.Fatalf("count allocated %v", allocs)
	}
}
