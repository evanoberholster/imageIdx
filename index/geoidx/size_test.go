package geoidx

import (
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// TestPersistentSize records the on-disk base size for a fixed image count so
// storage changes are visible. Run with -v.
func TestPersistentSize(t *testing.T) {
	if testing.Short() {
		t.Skip("persistent size check")
	}
	const n = 1_000_000
	dir := t.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]Entry, n)
	for i := range entries {
		lat := testLat + float64(i%1000)*0.0004
		lng := testLng + float64(i/1000%1000)*0.0004
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: common.ImageID(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		t.Fatal(err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	size := storage.DirSize(dir, "manifest", "base.*.dat")
	t.Logf("geoidx persistent 1M uniform: %d bytes (%d B/image)", size, size/uint64(n))
}
