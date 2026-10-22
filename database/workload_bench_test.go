package database

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/internal/bench"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/golang/geo/s2"
)

const (
	photoCameraNS = 201
	photoRatingNS = 202
	photoTagsNS   = 203
	photoAINS     = 204
	photoFavNS    = 205
	photoTimeNS   = 211
	photoGeoNS    = 212

	photoStateCount = 256
)

type photoState struct {
	camera    byte
	rating    byte
	tags      [8]byte
	ai        byte
	favorite  bool
	timestamp uint64
	geo       uint64
}

func photoDefinitions(t testing.TB) *schema.Definitions {
	t.Helper()
	defs := schema.NewDefinitions()
	for _, spec := range []struct {
		ns   schema.Namespace
		name string
		kind schema.NamespaceKind
	}{
		{photoCameraNS, "camera", schema.KindBitmapEnum},
		{photoRatingNS, "rating", schema.KindBitmapEnum},
		{photoTagsNS, "tags", schema.KindBitmapEnum},
		{photoAINS, "ai", schema.KindBitmapEnum},
		{photoFavNS, "favorite", schema.KindBitmapBool},
	} {
		def, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: spec.ns, Kind: spec.kind}, spec.name, schema.EntityImage)
		if err != nil {
			t.Fatal(err)
		}
		if err := defs.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	timeDef := timeidx.Spec{Namespace: photoTimeNS, Name: "captured"}.NamespaceDef()
	timeDef.Entity = schema.EntityImage
	if err := defs.Register(timeDef); err != nil {
		t.Fatal(err)
	}
	if err := defs.Register(schema.NamespaceDef{Namespace: photoGeoNS, Name: "gps", Kind: schema.KindGeo, Entity: schema.EntityImage}); err != nil {
		t.Fatal(err)
	}
	return defs
}

func photoPaths(b testing.TB) (string, string) {
	b.Helper()
	root := b.TempDir()
	data, idxPath := root+"/data", root+"/index"
	idx, err := index.Create(idxPath, index.Spec{Schema: photoDefinitions(b)})
	if err != nil {
		b.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		b.Fatal(err)
	}
	return data, idxPath
}

func photoProjection(id uint64, state photoState) []byte {
	out := make([]byte, 36)
	binary.BigEndian.PutUint64(out[0:8], id)
	out[8] = state.camera
	out[9] = state.rating
	copy(out[10:18], state.tags[:])
	out[18] = state.ai
	if state.favorite {
		out[19] = 1
	}
	binary.BigEndian.PutUint64(out[20:28], state.timestamp)
	binary.BigEndian.PutUint64(out[28:36], state.geo)
	return out
}

func decodePhoto(p []byte) (index.Entry, error) {
	if len(p) != 36 {
		return index.Entry{}, ErrCorruptJournal
	}
	return index.Entry{
		ValueID: binary.BigEndian.Uint64(p[0:8]),
		Bitmaps: []index.BitmapValue{
			{Namespace: photoCameraNS, Value: bitmapidx.EnumBytesValue(p[8:9])},
			{Namespace: photoRatingNS, Value: bitmapidx.EnumBytesValue(p[9:10])},
			{Namespace: photoTagsNS, Value: bitmapidx.EnumBytesValue(p[10:18])},
			{Namespace: photoAINS, Value: bitmapidx.EnumBytesValue(p[18:19])},
			{Namespace: photoFavNS, Value: bitmapidx.BoolValue(p[19] != 0)},
		},
		Times: []index.TimeValue{{Namespace: photoTimeNS, Timestamp: binary.BigEndian.Uint64(p[20:28])}},
		Geos:  []index.GeoValue{{Namespace: photoGeoNS, Key: geoidx.S2Key(binary.BigEndian.Uint64(p[28:36]))}},
	}, nil
}

func photoStateFor(id uint64) photoState {
	rng := bench.NewRNG(int64(id))
	return photoState{
		camera:    byte('a' + rng.Intn(8)),
		rating:    byte('1' + rng.Intn(5)),
		tags:      [8]byte{byte('a' + rng.Intn(8)), byte('a' + rng.Intn(8))},
		ai:        byte('a' + rng.Intn(8)),
		favorite:  rng.Intn(2) == 1,
		timestamp: 1_600_000_000 + id*37,
		geo:       photoGeoKey(rng.Intn(3)),
	}
}

func photoGeoKey(which int) uint64 {
	lat := []float64{37.0, 38.0, 39.0}[which]
	lng := []float64{-122.0, -121.0, -120.0}[which]
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(lat, lng)).Parent(geoidx.S2Level)
	return uint64(geoidx.EncodeKey(uint64(cell)))
}

// BenchmarkPhotoMetadataWorkloads is a deterministic logical-write matrix. The
// journal-B/op and store-B/op metrics are application-level byte counts from
// Stats, not device writes or fsync counts.
func BenchmarkPhotoMetadataWorkloads(b *testing.B) {
	workloads := []string{"insert", "rating", "tags", "gps", "ai", "unchanged"}
	for _, workload := range workloads {
		for _, size := range []int{1, 16, 32, 64} {
			b.Run(fmt.Sprintf("%s/batch-%d", workload, size), func(b *testing.B) {
				data, idxPath := photoPaths(b)
				db, err := Open(data, idxPath, decodePhoto, Options{BatchSize: 64})
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				states := make([]photoState, photoStateCount+1)
				if workload != "insert" {
					b.StopTimer()
					for start := 1; start <= photoStateCount; start += 64 {
						n := min(64, photoStateCount-start+1)
						mutations := make([]Mutation, n)
						for i := range mutations {
							id := uint64(start + i)
							states[id] = photoStateFor(id)
							mutations[i] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: photoProjection(id, states[id])}
						}
						g, err := db.Commit(mutations...)
						if err != nil {
							b.Fatal(err)
						}
						if err := db.WaitIndexed(context.Background(), g); err != nil {
							b.Fatal(err)
						}
					}
					b.StartTimer()
				}
				mutations := make([]Mutation, size)
				beforeStats := db.Stats()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for j := range mutations {
						var id uint64
						var state photoState
						if workload == "insert" {
							id = uint64(i*size + j + 1)
							state = photoStateFor(id)
						} else {
							id = uint64((i*size+j)%photoStateCount + 1)
							state = states[id]
							switch workload {
							case "rating":
								state.rating = byte('1' + (int(state.rating)-'1'+1)%5)
							case "tags":
								state.tags[0] = byte('a' + (int(state.tags[0])-'a'+1)%8)
							case "gps":
								state.geo = photoGeoKey((int(state.geo) + 1) % 3)
							case "ai":
								state.ai = byte('a' + (int(state.ai)-'a'+1)%8)
							}
							states[id] = state
						}
						mutations[j] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: photoProjection(id, state)}
					}
					g, err := db.Commit(mutations...)
					if err != nil {
						b.Fatal(err)
					}
					if err := db.WaitIndexed(context.Background(), g); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				stats := db.Stats()
				ops := float64(max(b.N*size, 1))
				b.ReportMetric(float64(stats.JournalBytes-beforeStats.JournalBytes)/ops, "journal-B/op")
				b.ReportMetric(float64(stats.Store.ValueBytes-beforeStats.Store.ValueBytes)/ops, "store-B/op")
				b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "images/s")
			})
		}
	}
}

// BenchmarkPhotoMetadataLatency records commit→indexed lag and a small mixed
// query latency distribution. It uses 1K live IDs by default so a routine
// benchmark remains bounded; startup/rebuild benchmarks cover 10K/100K.
func BenchmarkPhotoMetadataLatency(b *testing.B) {
	workloads := []string{"rating", "tags", "gps", "ai", "unchanged"}
	for _, workload := range workloads {
		for _, size := range []int{1, 16, 32, 64} {
			b.Run(fmt.Sprintf("%s/batch-%d", workload, size), func(b *testing.B) {
				data, idxPath := photoPaths(b)
				db, err := Open(data, idxPath, decodePhoto, Options{BatchSize: 64})
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				const stateCount = 1_000
				states := make([]photoState, stateCount+1)
				b.StopTimer()
				for start := 1; start <= stateCount; start += 64 {
					n := min(64, stateCount-start+1)
					mutations := make([]Mutation, n)
					for i := range mutations {
						id := uint64(start + i)
						states[id] = photoStateFor(id)
						mutations[i] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: photoProjection(id, states[id])}
					}
					g, err := db.Commit(mutations...)
					if err != nil {
						b.Fatal(err)
					}
					if err := db.WaitIndexed(context.Background(), g); err != nil {
						b.Fatal(err)
					}
				}
				rating, _ := db.Index().Bitmap(photoRatingNS)
				lag := bench.NewRecorder(b.N)
				query := bench.NewRecorder(b.N * 4)
				mutations := make([]Mutation, size)
				beforeStats := db.Stats()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for j := range mutations {
						id := uint64((i*size+j)%stateCount + 1)
						state := states[id]
						switch workload {
						case "rating":
							state.rating = byte('1' + (int(state.rating)-'1'+1)%5)
						case "tags":
							state.tags[0] = byte('a' + (int(state.tags[0])-'a'+1)%8)
						case "gps":
							state.geo = photoGeoKey((int(state.geo) + 1) % 3)
						case "ai":
							state.ai = byte('a' + (int(state.ai)-'a'+1)%8)
						}
						states[id] = state
						mutations[j] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: photoProjection(id, state)}
					}
					start := time.Now()
					g, err := db.Commit(mutations...)
					if err != nil {
						b.Fatal(err)
					}
					if err := db.WaitIndexed(context.Background(), g); err != nil {
						b.Fatal(err)
					}
					lag.Record(time.Since(start))
					for _, id := range []uint64{1, stateCount / 4, stateCount / 2, stateCount} {
						start := time.Now()
						if _, err := rating.Contains(id, bitmapidx.EnumBytesValue([]byte{byte('1')})); err != nil {
							b.Fatal(err)
						}
						query.Record(time.Since(start))
					}
				}
				b.StopTimer()
				lag.Report(b, "commit-indexed")
				query.Report(b, "bitmap-query")
				stats := db.Stats()
				ops := float64(max(b.N*size, 1))
				b.ReportMetric(float64(stats.JournalBytes-beforeStats.JournalBytes)/ops, "journal-B/op")
				b.ReportMetric(float64(stats.Store.ValueBytes-beforeStats.Store.ValueBytes)/ops, "store-B/op")
				b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "images/s")
				// Derived file sizes and heap are reported after the timed loop.
				b.ReportMetric(float64(bench.HeapBytes()), "heap-B")
				b.ReportMetric(float64(bench.DiskBytes(b.TempDir())), "disk-B")
			})
		}
	}
}
