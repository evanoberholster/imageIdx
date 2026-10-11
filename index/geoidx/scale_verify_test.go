package geoidx

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"syscall"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/golang/geo/s2"
)

// peakRSSBytes returns the process peak resident set size in bytes.
func peakRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return ru.Maxrss
	}
	return ru.Maxrss * 1024
}

// TestScale20And30Million builds 20M and 30M indexes (memory and persistent) and
// runs range and nearest queries, verifying cardinality. It is opt-in
// (GEOIDX_SCALE=1) because it allocates gigabytes and writes hundreds of MB.
func TestScale20And30Million(t *testing.T) {
	if os.Getenv("GEOIDX_SCALE") == "" {
		t.Skip("set GEOIDX_SCALE=1 to run the 20M/30M scale check")
	}
	center := s2.LatLngFromDegrees(37.7749, -122.4194)
	ranges := CoverCap(nil, CapFromMeters(center, 2000), CoverOptions{MaxCells: 16})

	for _, n := range []int{20_000_000, 30_000_000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			rng := rand.New(rand.NewSource(1))
			entries := make([]Entry, n)
			for i := range entries {
				lat := 37.7749 + (rng.Float64()*2-1)*0.4
				lng := -122.4194 + (rng.Float64()*2-1)*0.4
				k, err := KeyFromLatLng(lat, lng)
				if err != nil {
					t.Fatal(err)
				}
				entries[i] = Entry{Key: k, ImageID: common.ImageID(i + 1)}
			}

			// Memory.
			mem, err := NewMemory(Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := mem.InsertBatch(entries); err != nil {
				t.Fatal(err)
			}
			if err := mem.Compact(); err != nil {
				t.Fatal(err)
			}
			got := mem.SearchRanges(nil, ranges)
			res, err := mem.Nearest(nil, 37.7749, -122.4194, 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := mem.Len(); got != uint64(n) {
				t.Fatalf("memory Len = %d, want %d", got, n)
			}
			t.Logf("memory n=%d shards=%d range=%d nearest=%d peakRSS=%.2fGB",
				n, len(mem.view.Load().shards), len(got), len(res), float64(peakRSSBytes())/(1<<30))
			_ = mem.Close()

			// Persistent single file.
			dir := t.TempDir()
			cfg := Config{MaxFileBytes: 3 << 30}
			p, err := New(dir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.InsertBatch(entries); err != nil {
				t.Fatal(err)
			}
			if err := p.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			pRange := p.SearchRanges(nil, ranges)
			pStats := p.StorageStats()
			if got := p.Len(); got != uint64(n) {
				t.Fatalf("persistent Len = %d, want %d", got, n)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if got := reopened.Len(); got != uint64(n) {
				t.Fatalf("reopened Len = %d, want %d", got, n)
			}
			if !validateDirectory(reopened.view.Load().shards) {
				t.Fatal("reopened directory invalid")
			}
			t.Logf("persistent n=%d shards=%d range=%d arena=%.2fGB live=%.2fGB free=%.2fGB runs=%d peakRSS=%.2fGB",
				n, len(reopened.view.Load().shards), len(pRange),
				float64(pStats.ArenaBytes)/(1<<30), float64(pStats.LiveBytes)/(1<<30),
				float64(pStats.FreeBytes)/(1<<30), pStats.FreeRuns, float64(peakRSSBytes())/(1<<30))
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
