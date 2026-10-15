package search

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index/geoidx"
)

// geoEntry is one coordinate used to seed a test geoidx index. Coordinates are
// converted to S2 keys; geoidx stores no latitude/longitude.
type geoEntry struct {
	id       uint64
	lat, lng float64
}

// newTestGeoNamespace builds a compacted in-memory geoidx index.
func newTestGeoNamespace(t testing.TB, ns schema.Namespace, entries []geoEntry) *geoidx.Index {
	t.Helper()
	index, err := geoidx.NewMemory(geoidx.Config{Namespace: ns})
	if err != nil {
		t.Fatalf("geoidx.NewMemory: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if len(entries) == 0 {
		return index
	}
	adds := make([]geoidx.Entry, len(entries))
	for i, entry := range entries {
		key, err := geoidx.KeyFromLatLng(entry.lat, entry.lng)
		if err != nil {
			t.Fatalf("geoidx.KeyFromLatLng: %v", err)
		}
		adds[i] = geoidx.Entry{Key: key, ImageID: entry.id}
	}
	if err := index.InsertBatch(adds); err != nil {
		t.Fatalf("geoidx.InsertBatch: %v", err)
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("geoidx.Compact: %v", err)
	}
	return index
}

// geoSameCell returns n entries and their shared cell ID for a coordinate.
func geoSameCell(lat, lng float64, n int) ([]geoEntry, uint64) {
	cell, _ := geoidx.CellIDFromLatLng(lat, lng)
	entries := make([]geoEntry, n)
	for i := range entries {
		entries[i] = geoEntry{id: uint64(i + 1), lat: lat, lng: lng}
	}
	return entries, cell
}
