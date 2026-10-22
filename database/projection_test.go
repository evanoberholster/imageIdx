package database

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestChangedProjectionSpecExample(t *testing.T) {
	before := index.Entry{
		ValueID: 7,
		Bitmaps: []index.BitmapValue{
			{Namespace: 101, Value: bitmapidx.EnumStringValue("Canon")},
			{Namespace: 102, Value: bitmapidx.IntValue(3)},
			{Namespace: 103, Value: bitmapidx.IntValue(400)},
		},
	}
	after := index.Entry{
		ValueID: 7,
		Bitmaps: []index.BitmapValue{
			{Namespace: 101, Value: bitmapidx.EnumStringValue("Canon")},
			{Namespace: 102, Value: bitmapidx.IntValue(4)},
			{Namespace: 103, Value: bitmapidx.IntValue(400)},
		},
	}
	old, next := changedProjection(before, after)
	if len(old.Bitmaps) != 1 || old.Bitmaps[0].Namespace != 102 || old.Bitmaps[0].Value.Int != 3 {
		t.Fatalf("unexpected removals: %+v", old.Bitmaps)
	}
	if len(next.Bitmaps) != 1 || next.Bitmaps[0].Namespace != 102 || next.Bitmaps[0].Value.Int != 4 {
		t.Fatalf("unexpected additions: %+v", next.Bitmaps)
	}
}

func TestChangedProjectionEmptyAndExhaustedSides(t *testing.T) {
	var empty index.Entry
	full := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{
		{Namespace: 1, Value: bitmapidx.EnumStringValue("a")},
		{Namespace: 1, Value: bitmapidx.EnumStringValue("b")},
	}}
	removed, added := changedProjection(empty, full)
	if len(removed.Bitmaps) != 0 || len(added.Bitmaps) != 2 {
		t.Fatalf("empty->full: removed=%d added=%d", len(removed.Bitmaps), len(added.Bitmaps))
	}
	removed, added = changedProjection(full, empty)
	if len(removed.Bitmaps) != 2 || len(added.Bitmaps) != 0 {
		t.Fatalf("full->empty: removed=%d added=%d", len(removed.Bitmaps), len(added.Bitmaps))
	}
}

func TestChangedProjectionDuplicatesUseSetSemantics(t *testing.T) {
	a := index.BitmapValue{Namespace: 1, Value: bitmapidx.EnumStringValue("a")}
	before := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{a, a}}
	after := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{a}}
	removed, added := changedProjection(before, after)
	if len(removed.Bitmaps) != 0 || len(added.Bitmaps) != 0 {
		t.Fatalf("duplicate identical associations produced %d removals and %d additions", len(removed.Bitmaps), len(added.Bitmaps))
	}
	removed, added = changedProjection(before, index.Entry{ValueID: 1})
	if len(removed.Bitmaps) != 1 || len(added.Bitmaps) != 0 {
		t.Fatalf("removing duplicate associations produced %d removals and %d additions", len(removed.Bitmaps), len(added.Bitmaps))
	}
	removed, added = changedProjection(index.Entry{ValueID: 1}, index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{a, a}})
	if len(removed.Bitmaps) != 0 || len(added.Bitmaps) != 1 {
		t.Fatalf("adding duplicate associations produced %d removals and %d additions", len(removed.Bitmaps), len(added.Bitmaps))
	}
}

func TestChangedProjectionVectorFloatSemantics(t *testing.T) {
	nan := float32(math.NaN())
	sameNaN := index.VectorValue{Namespace: 1, Element: 4, Float32: []float32{nan}}
	before := index.Entry{ValueID: 1, Vectors: []index.VectorValue{sameNaN}}
	after := index.Entry{ValueID: 1, Vectors: []index.VectorValue{sameNaN}}
	removed, added := changedProjection(before, after)
	if len(removed.Vectors) != 1 || len(added.Vectors) != 1 {
		t.Fatalf("NaN equality changed: removed=%d added=%d", len(removed.Vectors), len(added.Vectors))
	}

	posZero := index.VectorValue{Namespace: 1, Element: 4, Float32: []float32{0}}
	negZero := index.VectorValue{Namespace: 1, Element: 4, Float32: []float32{float32(math.Copysign(0, -1))}}
	removed, added = changedProjection(
		index.Entry{ValueID: 1, Vectors: []index.VectorValue{posZero}},
		index.Entry{ValueID: 1, Vectors: []index.VectorValue{negZero}},
	)
	if len(removed.Vectors) != 0 || len(added.Vectors) != 0 {
		t.Fatalf("signed zero equality changed: removed=%d added=%d", len(removed.Vectors), len(added.Vectors))
	}
}

func TestChangedProjectionMatchesReference(t *testing.T) {
	for _, n := range []int{0, 1, 4, 16, 64, 256} {
		before := mixedProjection(n, 11)
		after := before
		after = mutateProjection(after, n, 7)
		wantBefore, wantAfter := changedReference(before, after)
		gotBefore, gotAfter := changedProjection(before, after)
		assertProjectionEqual(t, "before", gotBefore, wantBefore)
		assertProjectionEqual(t, "after", gotAfter, wantAfter)
	}
}

func mixedProjection(n, seed int) index.Entry {
	entry := index.Entry{ValueID: 42}
	for i := 0; i < n; i++ {
		ns := schema.Namespace(100 + i%5)
		entry.Bitmaps = append(entry.Bitmaps, index.BitmapValue{
			Namespace: ns,
			Value:     bitmapidx.EnumStringValue(fmt.Sprintf("v-%d-%d", seed, i)),
		})
		if i%2 == 0 {
			entry.Times = append(entry.Times, index.TimeValue{Namespace: ns, Timestamp: uint64(seed*1000 + i)})
		}
		if i%3 == 0 {
			entry.Geos = append(entry.Geos, index.GeoValue{Namespace: ns, Key: geoidx.S2Key(seed*100 + i)})
		}
		if i%4 == 0 {
			var hash radixhash.Hash
			hash[0], hash[1] = byte(seed), byte(i)
			entry.Hashes = append(entry.Hashes, index.HashValue{Namespace: ns, Hash: hash})
		}
		if i%5 == 0 {
			entry.Sims = append(entry.Sims, index.SimValue{
				Namespace: ns,
				Record:    simhash.Record{PHash: uint64(seed*1000 + i), Quality: uint8(i)},
			})
		}
		if i%6 == 0 {
			entry.Vectors = append(entry.Vectors, index.VectorValue{
				Namespace: ns,
				Element:   vectoridx.ElementFloat32,
				Float32:   []float32{float32(seed), float32(i), 0.5},
			})
		}
	}
	return entry
}

func mutateProjection(entry index.Entry, n, seed int) index.Entry {
	for i := range entry.Bitmaps {
		if i%3 == 0 {
			entry.Bitmaps[i].Value = bitmapidx.EnumStringValue(fmt.Sprintf("v-%d-%d", seed, i))
		}
	}
	for i := range entry.Times {
		if i%2 == 0 {
			entry.Times[i].Timestamp += 1
		}
	}
	for i := range entry.Hashes {
		entry.Hashes[i].Hash[2]++
	}
	for i := range entry.Sims {
		entry.Sims[i].Record.PHash++
	}
	for i := range entry.Vectors {
		entry.Vectors[i].Float32[1]++
	}
	return entry
}

func assertProjectionEqual(t *testing.T, label string, got, want index.Entry) {
	t.Helper()
	if got.ValueID != want.ValueID {
		t.Fatalf("%s ValueID = %d, want %d", label, got.ValueID, want.ValueID)
	}
	if !sameTimeSlices(got.Times, want.Times) ||
		!sameGeoSlices(got.Geos, want.Geos) ||
		!sameSimSlices(got.Sims, want.Sims) ||
		!sameHashSlices(got.Hashes, want.Hashes) ||
		!sameVectorSlices(got.Vectors, want.Vectors) ||
		!sameBitmapSlices(got.Bitmaps, want.Bitmaps) {
		t.Fatalf("%s mismatch:\n got=%+v\nwant=%+v", label, got, want)
	}
}

func sameTimeSlices(a, b []index.TimeValue) bool {
	a = append([]index.TimeValue(nil), a...)
	b = append([]index.TimeValue(nil), b...)
	slices.SortFunc(a, compareTime)
	slices.SortFunc(b, compareTime)
	return slices.EqualFunc(a, b, sameTime)
}

func sameGeoSlices(a, b []index.GeoValue) bool {
	a = append([]index.GeoValue(nil), a...)
	b = append([]index.GeoValue(nil), b...)
	slices.SortFunc(a, compareGeo)
	slices.SortFunc(b, compareGeo)
	return slices.EqualFunc(a, b, sameGeo)
}

func sameSimSlices(a, b []index.SimValue) bool {
	a = append([]index.SimValue(nil), a...)
	b = append([]index.SimValue(nil), b...)
	slices.SortFunc(a, compareSim)
	slices.SortFunc(b, compareSim)
	return slices.EqualFunc(a, b, sameSim)
}

func sameHashSlices(a, b []index.HashValue) bool {
	a = append([]index.HashValue(nil), a...)
	b = append([]index.HashValue(nil), b...)
	slices.SortFunc(a, compareHash)
	slices.SortFunc(b, compareHash)
	return slices.EqualFunc(a, b, sameHash)
}

func sameVectorSlices(a, b []index.VectorValue) bool {
	a = append([]index.VectorValue(nil), a...)
	b = append([]index.VectorValue(nil), b...)
	slices.SortFunc(a, compareVector)
	slices.SortFunc(b, compareVector)
	return slices.EqualFunc(a, b, sameVector)
}

func sameBitmapSlices(a, b []index.BitmapValue) bool {
	a = append([]index.BitmapValue(nil), a...)
	b = append([]index.BitmapValue(nil), b...)
	slices.SortFunc(a, compareBitmap)
	slices.SortFunc(b, compareBitmap)
	return slices.EqualFunc(a, b, sameBitmap)
}

func changedReference(before, after index.Entry) (index.Entry, index.Entry) {
	old, next := before, after
	before.Times = changedRef(old.Times, next.Times, sameTime)
	after.Times = changedRef(next.Times, old.Times, sameTime)
	before.Geos = changedRef(old.Geos, next.Geos, sameGeo)
	after.Geos = changedRef(next.Geos, old.Geos, sameGeo)
	before.Sims = changedRef(old.Sims, next.Sims, sameSim)
	after.Sims = changedRef(next.Sims, old.Sims, sameSim)
	before.Hashes = changedRef(old.Hashes, next.Hashes, sameHash)
	after.Hashes = changedRef(next.Hashes, old.Hashes, sameHash)
	before.Vectors = changedRef(old.Vectors, next.Vectors, sameVector)
	after.Vectors = changedRef(next.Vectors, old.Vectors, sameVector)
	before.Bitmaps = changedRef(old.Bitmaps, next.Bitmaps, sameBitmap)
	after.Bitmaps = changedRef(next.Bitmaps, old.Bitmaps, sameBitmap)
	return before, after
}

func changedRef[T any](values, other []T, equal func(T, T) bool) []T {
	var dst []T
	for i, value := range values {
		same := false
		for _, candidate := range other {
			if equal(value, candidate) {
				same = true
				break
			}
		}
		if same {
			if dst == nil {
				dst = append(make([]T, 0, len(values)), values[:i]...)
			}
		} else if dst != nil {
			dst = append(dst, value)
		}
	}
	if dst == nil {
		return values
	}
	return dst
}

func projectionBenchFixture(n int, changed int) (index.Entry, index.Entry) {
	before := index.Entry{ValueID: 1}
	for i := 0; i < n; i++ {
		before.Bitmaps = append(before.Bitmaps, index.BitmapValue{
			Namespace: schema.Namespace(100 + i%16),
			Value:     bitmapidx.EnumStringValue(fmt.Sprintf("value-%d", i)),
		})
	}
	after := before
	if changed == -1 {
		return before, after
	}
	if changed == 0 {
		for i := range after.Bitmaps {
			after.Bitmaps[i].Value = bitmapidx.EnumStringValue(fmt.Sprintf("changed-%d", i))
		}
		return before, after
	}
	after.Bitmaps[0].Value = bitmapidx.EnumStringValue("changed-0")
	return before, after
}

func BenchmarkProjectionDiffReference(b *testing.B) {
	for _, n := range []int{1, 4, 8, 16, 64, 256} {
		for _, changed := range []int{-1, 1, 0} {
			name := fmt.Sprintf("n=%d/changed=%d", n, changed)
			b.Run(name, func(b *testing.B) {
				before, after := projectionBenchFixture(n, changed)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					changedReference(before, after)
				}
			})
		}
	}
}

func BenchmarkProjectionDiffSortMerge(b *testing.B) {
	for _, n := range []int{1, 4, 8, 16, 64, 256} {
		for _, changed := range []int{-1, 1, 0} {
			name := fmt.Sprintf("n=%d/changed=%d", n, changed)
			b.Run(name, func(b *testing.B) {
				before, after := projectionBenchFixture(n, changed)
				var scratch projectionScratch
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					changedProjectionScratch(before, after, &scratch)
				}
			})
		}
	}
}

func TestChangedProjectionUnchangedOneAndAll(t *testing.T) {
	before := mixedProjection(64, 5)
	removed, added := changedProjection(before, before)
	if len(removed.Bitmaps)+len(removed.Times)+len(removed.Geos)+len(removed.Hashes)+len(removed.Sims)+len(removed.Vectors) != 0 ||
		len(added.Bitmaps)+len(added.Times)+len(added.Geos)+len(added.Hashes)+len(added.Sims)+len(added.Vectors) != 0 {
		t.Fatal("identical projection produced changes")
	}

	one := before
	one.Bitmaps = append([]index.BitmapValue(nil), before.Bitmaps...)
	one.Bitmaps[0].Value = bitmapidx.EnumStringValue("one-change")
	removed, added = changedProjection(before, one)
	if len(removed.Bitmaps)+len(added.Bitmaps) != 2 {
		t.Fatalf("one change produced %d removals/%d additions", len(removed.Bitmaps), len(added.Bitmaps))
	}

	all := before
	all.Bitmaps = append([]index.BitmapValue(nil), before.Bitmaps...)
	for i := range all.Bitmaps {
		all.Bitmaps[i].Value = bitmapidx.EnumStringValue(fmt.Sprintf("all-%d", i))
	}
	removed, added = changedProjection(before, all)
	if len(removed.Bitmaps) != len(before.Bitmaps) || len(added.Bitmaps) != len(before.Bitmaps) {
		t.Fatalf("all change produced %d removals/%d additions", len(removed.Bitmaps), len(added.Bitmaps))
	}
}

func TestChangedProjectionDoesNotSortInput(t *testing.T) {
	a := index.BitmapValue{Namespace: 1, Value: bitmapidx.EnumStringValue("a")}
	b := index.BitmapValue{Namespace: 1, Value: bitmapidx.EnumStringValue("b")}
	c := index.BitmapValue{Namespace: 1, Value: bitmapidx.EnumStringValue("c")}
	before := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{c, a, b}}
	after := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{b, c, a}}
	wantBefore := append([]index.BitmapValue(nil), before.Bitmaps...)
	wantAfter := append([]index.BitmapValue(nil), after.Bitmaps...)
	var scratch projectionScratch
	changedProjectionScratch(before, after, &scratch)
	if !slices.EqualFunc(before.Bitmaps, wantBefore, sameBitmap) || !slices.EqualFunc(after.Bitmaps, wantAfter, sameBitmap) {
		t.Fatal("projection diff destructively sorted its input")
	}
}
