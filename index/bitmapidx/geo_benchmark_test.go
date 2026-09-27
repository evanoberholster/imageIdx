package bitmapidx

import (
	"math/rand"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

type geoBenchCity struct {
	name     string
	lat, lng float64
}

var geoBenchCities = []geoBenchCity{
	{"Paris", 48.8566, 2.3522},
	{"London", 51.5074, -0.1278},
	{"New York", 40.7128, -74.0060},
	{"Tokyo", 35.6762, 139.6503},
	{"Berlin", 52.5200, 13.4050},
	{"Madrid", 40.4168, -3.7038},
	{"Rome", 41.9028, 12.4964},
	{"Amsterdam", 52.3676, 4.9041},
	{"Vienna", 48.2082, 16.3738},
	{"Prague", 50.0755, 14.4378},
	{"Lisbon", 38.7223, -9.1393},
	{"Dublin", 53.3498, -6.2603},
	{"Barcelona", 41.3874, 2.1686},
	{"Munich", 48.1351, 11.5820},
	{"Milan", 45.4642, 9.1900},
	{"Brussels", 50.8476, 4.3572},
	{"Copenhagen", 55.6761, 12.5683},
	{"Stockholm", 59.3293, 18.0686},
	{"Oslo", 59.9139, 10.7522},
	{"Helsinki", 60.1699, 24.9384},
	{"Warsaw", 52.2297, 21.0122},
	{"Budapest", 47.4979, 19.0402},
	{"San Francisco", 37.7749, -122.4194},
	{"Sydney", -33.8688, 151.2093},
}

// benchmarkGeoNamespace builds a KindBitmapGeo namespace with perCity images scattered
// around each benchmark city and returns the namespace, one level-8 query cell per
// city (the coarse cell used by "search Paris" style queries), and the
// canonical coordinate lookup used by Nearest.
func benchmarkGeoNamespace(b *testing.B, perCity int) (*Index, []uint64, GeoCoordinateLookup) {
	b.Helper()
	rng := rand.New(rand.NewSource(42))
	total := perCity * len(geoBenchCities)
	entries := make([]GeoEntry, 0, total)
	coordinates := make(map[common.ImageID][2]float64, total)
	id := common.ImageID(1)
	for _, city := range geoBenchCities {
		for range perCity {
			latitude := city.lat + (rng.Float64()-0.5)*0.3
			longitude := city.lng + (rng.Float64()-0.5)*0.3
			entries = append(entries, GeoEntry{ImageID: id, Latitude: latitude, Longitude: longitude})
			coordinates[id] = [2]float64{latitude, longitude}
			id++
		}
	}
	namespace, err := NewGeoNamespace(common.NamespaceLocation, entries, NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = namespace.Close() })

	cells := make([]uint64, len(geoBenchCities))
	for index, city := range geoBenchCities {
		cells[index] = uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(city.lat, city.lng)).Parent(8))
	}
	lookup := func(id common.ImageID) (float64, float64, bool) {
		coordinate, ok := coordinates[id]
		return coordinate[0], coordinate[1], ok
	}
	return namespace, cells, lookup
}

// BenchmarkGeoCitySearch measures city queries over a 24-city distribution.
// Each query covers a level-8 S2 cell and unions the occupied level-16
// buckets inside it.
func BenchmarkGeoCitySearch(b *testing.B) {
	const perCity = 5_000
	namespace, cells, _ := benchmarkGeoNamespace(b, perCity)
	for index, city := range geoBenchCities[:4] {
		ranges, err := RangesFromCellIDs(nil, cells[index:index+1])
		if err != nil {
			b.Fatal(err)
		}
		match, err := namespace.GeoRanges(ranges)
		if err != nil {
			b.Fatal(err)
		}
		if match.Cardinality() == 0 {
			b.Fatalf("%s query matched nothing", city.name)
		}
		neighborhood, err := RangesFromCellIDs(nil, []uint64{uint64(s2.CellID(cells[index]).Parent(10))})
		if err != nil {
			b.Fatal(err)
		}

		b.Run(city.name+"/ranges", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_, _ = namespace.GeoRanges(ranges)
			}
		})
		b.Run(city.name+"/append", func(b *testing.B) {
			dst := make([]common.ImageID, 0, int(match.Cardinality()))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, _ := namespace.GeoRanges(ranges)
				dst = result.AppendTo(dst[:0])
			}
			_ = dst
		})
		// The probe path targets a small neighborhood with a short candidate
		// list, matching the planner's verify gate; large candidate sets take
		// the collect path instead.
		b.Run(city.name+"/filter", func(b *testing.B) {
			candidates := make([]common.ImageID, 0, 128)
			for id := common.ImageID(1); len(candidates) < 128; id += 937 {
				candidates = append(candidates, id)
			}
			dst := make([]common.ImageID, 0, len(candidates))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				dst, _ = namespace.FilterGeoRanges(dst[:0], candidates, neighborhood)
			}
			_ = dst
		})
		b.Run(city.name+"/collect", func(b *testing.B) {
			m := roar.NewMutableBitmap()
			dst := make([]common.ImageID, 0, int(match.Cardinality()))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.Reset()
				namespace.CollectGeoRanges(m, ranges)
				dst = m.AppendTo(dst[:0])
			}
			_ = dst
		})
	}
}

// BenchmarkGeoCityNearest measures exact top-K over a 24-city distribution.
// Nearest scans the immutable view, so allocation reflects the full ID
// materialization.
func BenchmarkGeoCityNearest(b *testing.B) {
	namespace, _, lookup := benchmarkGeoNamespace(b, 500)
	for _, city := range geoBenchCities[:4] {
		b.Run(city.name, func(b *testing.B) {
			dst := make([]NearestResult, 0, 16)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				dst = namespace.Nearest(dst[:0], city.lat, city.lng, 10, lookup)
			}
			if len(dst) != 10 {
				b.Fatalf("Nearest returned %d results", len(dst))
			}
		})
	}
}
