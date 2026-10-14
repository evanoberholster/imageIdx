package database

import (
	"bytes"
	"slices"

	"github.com/evanoberholster/imageIdx/index"
)

// changedProjection removes associations present unchanged on both sides.
// Unlike index.Change alone, this avoids rewriting unchanged timestamp,
// geographic, similarity and vector values during metadata-only updates.
func changedProjection(before, after index.Entry) (index.Entry, index.Entry) {
	old, next := before, after
	before.Times = changed(old.Times, next.Times, sameTime)
	after.Times = changed(next.Times, old.Times, sameTime)
	before.Geos = changed(old.Geos, next.Geos, sameGeo)
	after.Geos = changed(next.Geos, old.Geos, sameGeo)
	before.Sims = changed(old.Sims, next.Sims, sameSim)
	after.Sims = changed(next.Sims, old.Sims, sameSim)
	before.Hashes = changed(old.Hashes, next.Hashes, sameHash)
	after.Hashes = changed(next.Hashes, old.Hashes, sameHash)
	before.Vectors = changed(old.Vectors, next.Vectors, sameVector)
	after.Vectors = changed(next.Vectors, old.Vectors, sameVector)
	before.Bitmaps = changed(old.Bitmaps, next.Bitmaps, sameBitmap)
	after.Bitmaps = changed(next.Bitmaps, old.Bitmaps, sameBitmap)
	return before, after
}

func changed[T any](values, other []T, equal func(T, T) bool) []T {
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

func sameTime(a, b index.TimeValue) bool { return a == b }
func sameGeo(a, b index.GeoValue) bool   { return a == b }
func sameSim(a, b index.SimValue) bool   { return a == b }
func sameHash(a, b index.HashValue) bool { return a == b }
func sameVector(a, b index.VectorValue) bool {
	return a.Namespace == b.Namespace && a.Element == b.Element && slices.Equal(a.Float32, b.Float32) &&
		slices.Equal(a.Int8, b.Int8) && slices.Equal(a.Uint8, b.Uint8) &&
		slices.Equal(a.Int16, b.Int16) && slices.Equal(a.Uint16, b.Uint16)
}
func sameBitmap(a, b index.BitmapValue) bool {
	x, y := a.Value, b.Value
	return a.Namespace == b.Namespace && x.Kind == y.Kind && x.Int == y.Int && x.Bool == y.Bool && bytes.Equal(x.Bytes, y.Bytes)
}
