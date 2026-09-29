//go:build !race

package timeidx

import (
	"math"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/bench"
)

// TestTimeSearchNoAlloc documents the reader-side allocation invariant:
// once the caller's dst has capacity for the candidate set, Index.Search
// appends ImageIDs into dst and performs no heap allocation, so a hot query
// path can run without touching the allocator. The assertion is excluded
// from race builds because the race detector's shadow bookkeeping can
// perturb allocation counts (same convention as search/allocation_test.go).
func TestTimeSearchNoAlloc(t *testing.T) {
	const n = 100_000
	timestamps := bench.Timestamps(7, n, bench.TimeUniform)
	entries := make([]Entry, n)
	for i, timestamp := range timestamps {
		entries[i] = Entry{Timestamp: timestamp, ImageID: common.ValueID(i + 1)}
	}
	index, err := NewMemory(Spec{Entries: entries}, Config{DeltaCapacity: 4_096, BlockSize: 4_096})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()

	full := index.Search(nil, 0, math.MaxUint64)
	if len(full) != n {
		t.Fatalf("full-range search = %d candidates, want %d", len(full), n)
	}
	dst := make([]common.ValueID, 0, len(full))
	if allocs := testing.AllocsPerRun(100, func() {
		dst = index.Search(dst[:0], 0, math.MaxUint64)
	}); allocs != 0 {
		t.Fatalf("full-range search allocated %v allocs/run, want 0", allocs)
	}

	// A miss must not allocate either. Search filters by 24-bit timestamp
	// marker, so the window must end on a marker boundary below the
	// minimum timestamp, not merely below it, to exclude its bucket.
	minimum := slices.Min(timestamps)
	missEnd := (minimum >> TimestampShift) << TimestampShift
	if allocs := testing.AllocsPerRun(100, func() {
		dst = index.Search(dst[:0], 0, missEnd)
	}); allocs != 0 {
		t.Fatalf("missing-range search allocated %v allocs/run, want 0", allocs)
	}
	if len(dst) != 0 {
		t.Fatalf("missing-range search = %d candidates, want 0", len(dst))
	}
}
