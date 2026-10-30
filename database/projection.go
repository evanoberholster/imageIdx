package database

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/evanoberholster/imageIdx/index"
)

// projectionScratch owns the reusable typed buffers used by the deterministic
// projection diff. All slices are writer-owned and reset between journal
// batches; output slices remain valid until the index Apply call returns.
type projectionScratch struct {
	timesBefore, timesAfter      []index.TimeValue
	timesRemoved, timesAdded     []index.TimeValue
	geosBefore, geosAfter        []index.GeoValue
	geosRemoved, geosAdded       []index.GeoValue
	simsBefore, simsAfter        []index.SimValue
	simsRemoved, simsAdded       []index.SimValue
	hashesBefore, hashesAfter    []index.HashValue
	hashesRemoved, hashesAdded   []index.HashValue
	vectorsBefore, vectorsAfter  []index.VectorValue
	vectorsRemoved, vectorsAdded []index.VectorValue
	bitmapsBefore, bitmapsAfter  []index.BitmapValue
	bitmapsRemoved, bitmapsAdded []index.BitmapValue
}

func (s *projectionScratch) reset() {
	clear(s.vectorsBefore)
	clear(s.vectorsAfter)
	clear(s.vectorsRemoved)
	clear(s.vectorsAdded)
	clear(s.bitmapsBefore)
	clear(s.bitmapsAfter)
	clear(s.bitmapsRemoved)
	clear(s.bitmapsAdded)

	s.timesBefore = s.timesBefore[:0]
	s.timesAfter = s.timesAfter[:0]
	s.timesRemoved = s.timesRemoved[:0]
	s.timesAdded = s.timesAdded[:0]
	s.geosBefore = s.geosBefore[:0]
	s.geosAfter = s.geosAfter[:0]
	s.geosRemoved = s.geosRemoved[:0]
	s.geosAdded = s.geosAdded[:0]
	s.simsBefore = s.simsBefore[:0]
	s.simsAfter = s.simsAfter[:0]
	s.simsRemoved = s.simsRemoved[:0]
	s.simsAdded = s.simsAdded[:0]
	s.hashesBefore = s.hashesBefore[:0]
	s.hashesAfter = s.hashesAfter[:0]
	s.hashesRemoved = s.hashesRemoved[:0]
	s.hashesAdded = s.hashesAdded[:0]
	s.vectorsBefore = s.vectorsBefore[:0]
	s.vectorsAfter = s.vectorsAfter[:0]
	s.vectorsRemoved = s.vectorsRemoved[:0]
	s.vectorsAdded = s.vectorsAdded[:0]
	s.bitmapsBefore = s.bitmapsBefore[:0]
	s.bitmapsAfter = s.bitmapsAfter[:0]
	s.bitmapsRemoved = s.bitmapsRemoved[:0]
	s.bitmapsAdded = s.bitmapsAdded[:0]
}

// changedProjection is the test/reference entry point that allocates its own
// scratch. The coordinator uses changedProjectionScratch with per-mutation
// reusable storage.
func changedProjection(before, after index.Entry) (index.Entry, index.Entry) {
	var scratch projectionScratch
	return changedProjectionScratch(before, after, &scratch)
}

func changedProjectionScratch(before, after index.Entry, s *projectionScratch) (index.Entry, index.Entry) {
	outBefore := index.Entry{ValueID: before.ValueID}
	outAfter := index.Entry{ValueID: after.ValueID}
	if outAfter.ValueID == 0 {
		outAfter.ValueID = before.ValueID
	}

	outBefore.Times, outAfter.Times = diffTimes(before.Times, after.Times, s)
	outBefore.Geos, outAfter.Geos = diffGeos(before.Geos, after.Geos, s)
	outBefore.Sims, outAfter.Sims = diffSims(before.Sims, after.Sims, s)
	outBefore.Hashes, outAfter.Hashes = diffHashes(before.Hashes, after.Hashes, s)
	outBefore.Vectors, outAfter.Vectors = diffVectors(before.Vectors, after.Vectors, s)
	outBefore.Bitmaps, outAfter.Bitmaps = diffBitmaps(before.Bitmaps, after.Bitmaps, s)
	return outBefore, outAfter
}

const smallDiffLimit = 2

func diffSmall[T any](before, after, removed, added []T, equal func(T, T) bool) ([]T, []T) {
	for _, value := range before {
		if !containsValue(after, value, equal) && !containsValue(removed, value, equal) {
			removed = append(removed, value)
		}
	}
	for _, value := range after {
		if !containsValue(before, value, equal) && !containsValue(added, value, equal) {
			added = append(added, value)
		}
	}
	return removed, added
}

func containsValue[T any](values []T, target T, equal func(T, T) bool) bool {
	for _, value := range values {
		if equal(value, target) {
			return true
		}
	}
	return false
}

func diffTimes(before, after []index.TimeValue, s *projectionScratch) ([]index.TimeValue, []index.TimeValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.timesRemoved[:0], s.timesAdded[:0], sameTime)
	}
	s.timesBefore = append(s.timesBefore[:0], before...)
	s.timesAfter = append(s.timesAfter[:0], after...)
	slices.SortFunc(s.timesBefore, compareTime)
	slices.SortFunc(s.timesAfter, compareTime)
	s.timesBefore = slices.CompactFunc(s.timesBefore, sameTime)
	s.timesAfter = slices.CompactFunc(s.timesAfter, sameTime)
	return mergeSorted(s.timesBefore, s.timesAfter, s.timesRemoved[:0], s.timesAdded[:0], compareTime, sameTime)
}

func diffGeos(before, after []index.GeoValue, s *projectionScratch) ([]index.GeoValue, []index.GeoValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.geosRemoved[:0], s.geosAdded[:0], sameGeo)
	}
	s.geosBefore = append(s.geosBefore[:0], before...)
	s.geosAfter = append(s.geosAfter[:0], after...)
	slices.SortFunc(s.geosBefore, compareGeo)
	slices.SortFunc(s.geosAfter, compareGeo)
	s.geosBefore = slices.CompactFunc(s.geosBefore, sameGeo)
	s.geosAfter = slices.CompactFunc(s.geosAfter, sameGeo)
	return mergeSorted(s.geosBefore, s.geosAfter, s.geosRemoved[:0], s.geosAdded[:0], compareGeo, sameGeo)
}

func diffSims(before, after []index.SimValue, s *projectionScratch) ([]index.SimValue, []index.SimValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.simsRemoved[:0], s.simsAdded[:0], sameSim)
	}
	s.simsBefore = append(s.simsBefore[:0], before...)
	s.simsAfter = append(s.simsAfter[:0], after...)
	slices.SortFunc(s.simsBefore, compareSim)
	slices.SortFunc(s.simsAfter, compareSim)
	s.simsBefore = slices.CompactFunc(s.simsBefore, sameSim)
	s.simsAfter = slices.CompactFunc(s.simsAfter, sameSim)
	return mergeSorted(s.simsBefore, s.simsAfter, s.simsRemoved[:0], s.simsAdded[:0], compareSim, sameSim)
}

func diffHashes(before, after []index.HashValue, s *projectionScratch) ([]index.HashValue, []index.HashValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.hashesRemoved[:0], s.hashesAdded[:0], sameHash)
	}
	s.hashesBefore = append(s.hashesBefore[:0], before...)
	s.hashesAfter = append(s.hashesAfter[:0], after...)
	slices.SortFunc(s.hashesBefore, compareHash)
	slices.SortFunc(s.hashesAfter, compareHash)
	s.hashesBefore = slices.CompactFunc(s.hashesBefore, sameHash)
	s.hashesAfter = slices.CompactFunc(s.hashesAfter, sameHash)
	return mergeSorted(s.hashesBefore, s.hashesAfter, s.hashesRemoved[:0], s.hashesAdded[:0], compareHash, sameHash)
}

func diffVectors(before, after []index.VectorValue, s *projectionScratch) ([]index.VectorValue, []index.VectorValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.vectorsRemoved[:0], s.vectorsAdded[:0], sameVector)
	}
	s.vectorsBefore = append(s.vectorsBefore[:0], before...)
	s.vectorsAfter = append(s.vectorsAfter[:0], after...)
	slices.SortFunc(s.vectorsBefore, compareVector)
	slices.SortFunc(s.vectorsAfter, compareVector)
	s.vectorsBefore = slices.CompactFunc(s.vectorsBefore, sameVector)
	s.vectorsAfter = slices.CompactFunc(s.vectorsAfter, sameVector)
	return mergeSorted(s.vectorsBefore, s.vectorsAfter, s.vectorsRemoved[:0], s.vectorsAdded[:0], compareVector, sameVector)
}

func diffBitmaps(before, after []index.BitmapValue, s *projectionScratch) ([]index.BitmapValue, []index.BitmapValue) {
	if len(before)+len(after) <= smallDiffLimit {
		return diffSmall(before, after, s.bitmapsRemoved[:0], s.bitmapsAdded[:0], sameBitmap)
	}
	s.bitmapsBefore = append(s.bitmapsBefore[:0], before...)
	s.bitmapsAfter = append(s.bitmapsAfter[:0], after...)
	slices.SortFunc(s.bitmapsBefore, compareBitmap)
	slices.SortFunc(s.bitmapsAfter, compareBitmap)
	s.bitmapsBefore = slices.CompactFunc(s.bitmapsBefore, sameBitmap)
	s.bitmapsAfter = slices.CompactFunc(s.bitmapsAfter, sameBitmap)
	return mergeSorted(s.bitmapsBefore, s.bitmapsAfter, s.bitmapsRemoved[:0], s.bitmapsAdded[:0], compareBitmap, sameBitmap)
}

func mergeSorted[T any](before, after, removed, added []T, compare func(T, T) int, equal func(T, T) bool) ([]T, []T) {
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		switch c := compare(before[i], after[j]); {
		case c < 0:
			removed = append(removed, before[i])
			i++
		case c > 0:
			added = append(added, after[j])
			j++
		default:
			// A total sort order can consider values equal that the semantic
			// equality contract does not (notably NaN vectors). Emit both when
			// that happens rather than silently suppressing a real change.
			if !equal(before[i], after[j]) {
				removed = append(removed, before[i])
				added = append(added, after[j])
			}
			i++
			j++
		}
	}
	removed = append(removed, before[i:]...)
	added = append(added, after[j:]...)
	return removed, added
}

func compareTime(a, b index.TimeValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return cmp.Compare(a.Timestamp, b.Timestamp)
}

func compareGeo(a, b index.GeoValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return cmp.Compare(a.Key, b.Key)
}

func compareSim(a, b index.SimValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Record.PHash, b.Record.PHash); c != 0 {
		return c
	}
	if c := bytes.Compare(a.Record.PDQ[:], b.Record.PDQ[:]); c != 0 {
		return c
	}
	return cmp.Compare(a.Record.Quality, b.Record.Quality)
}

func compareHash(a, b index.HashValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return bytes.Compare(a.Hash[:], b.Hash[:])
}

func compareVector(a, b index.VectorValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Element, b.Element); c != 0 {
		return c
	}
	if c := compareFloat32s(a.Float32, b.Float32); c != 0 {
		return c
	}
	if c := slices.Compare(a.Float16, b.Float16); c != 0 {
		return c
	}
	if c := slices.Compare(a.Int8, b.Int8); c != 0 {
		return c
	}
	if c := slices.Compare(a.Uint8, b.Uint8); c != 0 {
		return c
	}
	if c := slices.Compare(a.Int16, b.Int16); c != 0 {
		return c
	}
	return slices.Compare(a.Uint16, b.Uint16)
}

func compareFloat32s(a, b []float32) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if c := cmp.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

func compareBitmap(a, b index.BitmapValue) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Value.Kind, b.Value.Kind); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Value.Int, b.Value.Int); c != 0 {
		return c
	}
	if a.Value.Bool != b.Value.Bool {
		if !a.Value.Bool {
			return -1
		}
		return 1
	}
	return bytes.Compare(a.Value.Bytes, b.Value.Bytes)
}

func sameTime(a, b index.TimeValue) bool { return a == b }
func sameGeo(a, b index.GeoValue) bool   { return a == b }
func sameSim(a, b index.SimValue) bool   { return a == b }
func sameHash(a, b index.HashValue) bool { return a == b }
func sameVector(a, b index.VectorValue) bool {
	return a.Namespace == b.Namespace && a.Element == b.Element &&
		slices.Equal(a.Float32, b.Float32) && slices.Equal(a.Float16, b.Float16) &&
		slices.Equal(a.Int8, b.Int8) && slices.Equal(a.Uint8, b.Uint8) &&
		slices.Equal(a.Int16, b.Int16) && slices.Equal(a.Uint16, b.Uint16)
}
func sameBitmap(a, b index.BitmapValue) bool {
	x, y := a.Value, b.Value
	return a.Namespace == b.Namespace && x.Kind == y.Kind && x.Int == y.Int && x.Bool == y.Bool && bytes.Equal(x.Bytes, y.Bytes)
}
