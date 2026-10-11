package geoidx

import (
	"fmt"
	"math/rand"
	"os"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/golang/geo/s2"
)

// TestScale20And30Million builds 20M and 30M memory indexes and runs range and
// nearest queries. It is opt-in (GEOIDX_SCALE=1) because it allocates gigabytes.
func TestScale20And30Million(t *testing.T) {
	if os.Getenv("GEOIDX_SCALE") == "" {
		t.Skip("set GEOIDX_SCALE=1 to run the 20M/30M scale check")
	}
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
			index, err := NewMemory(Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer index.Close()
			if err := index.InsertBatch(entries); err != nil {
				t.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				t.Fatal(err)
			}
			ranges := CoverCap(nil, CapFromMeters(s2.LatLngFromDegrees(37.7749, -122.4194), 2000), CoverOptions{MaxCells: 16})
			e := index.SearchRanges(nil, ranges)
			res, err := index.Nearest(nil, 37.7749, -122.4194, 10)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("n=%d shards=%d range=%d nearest=%d firstDist=%d len=%d",
				n, len(index.view.Load().shards), len(e), len(res), res[0].Distance, index.Len())
		})
	}
}
