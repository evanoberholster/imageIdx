package roar

import (
	"math/bits"
	"testing"

	"github.com/evanoberholster/imageIdx/roar/internal/bitmapkernel"
)

// Temporary confirmation benchmarks for the v1 API review. This file is
// removed after numbers are collected; it is not part of the shipped suite.

var v1benchSink uint64

func v1benchViews(b *testing.B, n, keys int) []Bitmap {
	b.Helper()
	rng := rngForTest()
	pool := keyPool(rng, keys)
	views := make([]Bitmap, 0, n)
	for range n {
		bitmap, err := FromSorted(randomSetOver(rng, pool))
		if err != nil {
			b.Fatal(err)
		}
		views = append(views, bitmap)
	}
	return views
}

// Q1: batched OrManyInto vs sequential Or fold vs MutableBitmap incremental Or.
func BenchmarkV1OrManyVsFold(b *testing.B) {
	views := v1benchViews(b, 6, 24)
	b.Run("OrMany", func(b *testing.B) {
		b.ReportAllocs()
		builder := NewBuilder(0)
		for range b.N {
			builder.Reset()
			OrManyInto(builder, views...)
			v1benchSink += builder.cardinality
		}
	})
	b.Run("FoldOr", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			result := views[0]
			for _, bm := range views[1:] {
				result = Or(result, bm)
			}
			v1benchSink += result.Cardinality()
		}
	})
	b.Run("MutableOr", func(b *testing.B) {
		b.ReportAllocs()
		m := NewMutableBitmap()
		for range b.N {
			m.Reset()
			for _, bm := range views {
				m.Or(bm)
			}
			v1benchSink += m.Cardinality()
		}
	})
}

// Q1b: geoidx-like fan-in — many small cell bitmaps sharing few roar keys.
func BenchmarkV1OrManyCells(b *testing.B) {
	rng := rngForTest()
	var cells []Bitmap
	for range 16 {
		ids := denseSortedIDs(rng, 1)
		bitmap, err := FromSorted(ids)
		if err != nil {
			b.Fatal(err)
		}
		cells = append(cells, bitmap)
	}
	b.Run("OrMany", func(b *testing.B) {
		b.ReportAllocs()
		builder := NewBuilder(0)
		for range b.N {
			builder.Reset()
			OrManyInto(builder, cells...)
			v1benchSink += builder.cardinality
		}
	})
	b.Run("MutableOr", func(b *testing.B) {
		b.ReportAllocs()
		m := NewMutableBitmap()
		for range b.N {
			m.Reset()
			for _, bm := range cells {
				m.Or(bm)
			}
			v1benchSink += m.Cardinality()
		}
	})
}

// Q2: per-call stack cursors vs one pooled cursor with Reset.
func BenchmarkV1CursorStackVsPooled(b *testing.B) {
	views := v1benchViews(b, 4, 24)
	candidates := views[0].AppendTo(nil)
	if len(candidates) > 4096 {
		candidates = candidates[:4096]
	}
	b.Run("Stack", func(b *testing.B) {
		b.ReportAllocs()
		var out []uint64
		for range b.N {
			var cursorStack [8]Cursor
			cursors := cursorStack[:len(views)]
			for i, view := range views {
				cursors[i] = view.NewCursor()
			}
			out = out[:0]
			for _, id := range candidates {
				ok := true
				for i := range views {
					if !cursors[i].Contains(id) {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, id)
				}
			}
			v1benchSink += uint64(len(out))
		}
	})
	b.Run("Pooled", func(b *testing.B) {
		b.ReportAllocs()
		var out []uint64
		var pooled [8]Cursor
		for range b.N {
			cursors := pooled[:len(views)]
			for i, view := range views {
				cursors[i].Reset(view)
			}
			out = out[:0]
			for _, id := range candidates {
				ok := true
				for i := range views {
					if !cursors[i].Contains(id) {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, id)
				}
			}
			v1benchSink += uint64(len(out))
		}
	})
}

// Q3: fused op+cardinality vs op then separate cardinality pass.
func BenchmarkV1FusedVsAfter(b *testing.B) {
	rng := rngForTest()
	aIDs := denseSortedIDs(rng, 2)
	bIDs := denseSortedIDs(rng, 2)
	a, err := FromSorted(aIDs)
	if err != nil {
		b.Fatal(err)
	}
	c, err := FromSorted(bIDs)
	if err != nil {
		b.Fatal(err)
	}
	// Extract one dense container pair for a tight kernel loop.
	var aw, cw []uint64
	for _, entry := range a.keyEntries() {
		container := a.data[entry.Offset:]
		if ContainerType(containerHeaderAt(container).Type) == ContainerBitmap {
			aw = bitmapWordsAt(container)
			break
		}
	}
	for _, entry := range c.keyEntries() {
		container := c.data[entry.Offset:]
		if ContainerType(containerHeaderAt(container).Type) == ContainerBitmap {
			cw = bitmapWordsAt(container)
			break
		}
	}
	if len(aw) == 0 || len(cw) == 0 {
		b.Skip("no dense containers in fixture")
	}
	dst := make([]uint64, BitmapWords)
	b.Run("AndFused", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			v1benchSink += uint64(bitmapkernel.And(dst, aw, cw))
		}
	})
	b.Run("AndThenCount", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for i := range dst {
				dst[i] = aw[i] & cw[i]
			}
			var card uint64
			for _, w := range dst {
				card += uint64(bits.OnesCount64(w))
			}
			v1benchSink += card
		}
	})
	b.Run("OrFused", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			v1benchSink += uint64(bitmapkernel.Or(dst, aw, cw))
		}
	})
	b.Run("OrThenCount", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for i := range dst {
				dst[i] = aw[i] | cw[i]
			}
			var card uint64
			for _, w := range dst {
				card += uint64(bits.OnesCount64(w))
			}
			v1benchSink += card
		}
	})
	b.Run("AndNotFused", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			v1benchSink += uint64(bitmapkernel.AndNot(dst, aw, cw))
		}
	})
	b.Run("AndNotThenCount", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for i := range dst {
				dst[i] = aw[i] &^ cw[i]
			}
			var card uint64
			for _, w := range dst {
				card += uint64(bits.OnesCount64(w))
			}
			v1benchSink += card
		}
	})
}

// AndNot sparse-removal: baseCard-minus-cleared vs copy+clear+full recount.
func BenchmarkV1AndNotSparse(b *testing.B) {
	rng := rngForTest()
	denseIDs := denseSortedIDs(rng, 1)
	dense, err := FromSorted(denseIDs)
	if err != nil {
		b.Fatal(err)
	}
	var words []uint64
	var baseCard uint32
	for _, entry := range dense.keyEntries() {
		container := dense.data[entry.Offset:]
		if ContainerType(containerHeaderAt(container).Type) == ContainerBitmap {
			words = bitmapWordsAt(container)
			baseCard = containerHeaderAt(container).Card
			break
		}
	}
	if len(words) == 0 {
		b.Skip("no dense container in fixture")
	}
	values := make([]uint16, 64)
	for i := range values {
		values[i] = uint16(i * 997 % 65536)
	}
	dst := make([]uint64, BitmapWords)
	b.Run("BaseCardMinusCleared", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			v1benchSink += uint64(andNotWordsArray(dst, words, values, baseCard))
		}
	})
	b.Run("ClearThenFullRecount", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			copy(dst, words)
			for _, value := range values {
				dst[value>>6] &^= uint64(1) << (value & 63)
			}
			v1benchSink += bitmapkernel.Cardinality(dst)
		}
	})
}

// RemoveRange: word-mask clearing vs per-value loop.
func BenchmarkV1RemoveRange(b *testing.B) {
	rng := rngForTest()
	denseIDs := denseSortedIDs(rng, 1)
	dense, err := FromSorted(denseIDs)
	if err != nil {
		b.Fatal(err)
	}
	var words []uint64
	for _, entry := range dense.keyEntries() {
		container := dense.data[entry.Offset:]
		if ContainerType(containerHeaderAt(container).Type) == ContainerBitmap {
			words = bitmapWordsAt(container)
			break
		}
	}
	if len(words) == 0 {
		b.Skip("no dense container in fixture")
	}
	dst := make([]uint64, BitmapWords)
	const lo, hi = 100, 20000
	b.Run("WordMask", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			copy(dst, words)
			clearRangeWords(dst, lo, hi)
			v1benchSink += bitmapkernel.Cardinality(dst)
		}
	})
	b.Run("PerValueLoop", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			copy(dst, words)
			for v := lo; v < hi; v++ {
				dst[v>>6] &^= uint64(1) << (uint(v) & 63)
			}
			v1benchSink += bitmapkernel.Cardinality(dst)
		}
	})
}
