package roar

import (
	"github.com/evanoberholster/imageIdx/roar/internal/bitmapkernel"
)

// And returns the intersection of a and b as a new Bitmap. It is a
// convenience over AndInto plus Builder.Compact; prefer the Builder-based
// forms when the result buffer can be reused. Empty operands short-circuit
// to an empty Bitmap.
func And(a, b Bitmap) Bitmap {
	if a.IsEmpty() || b.IsEmpty() {
		return Bitmap{}
	}
	builder := NewBuilder(min(a.KeyCount(), b.KeyCount()))
	builder.reserveData(min(a.LenInBytes(), b.LenInBytes()))
	AndInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// Or returns the union of a and b as a new Bitmap. Empty operands are
// returned directly without building, since Bitmaps are immutable.
func Or(a, b Bitmap) Bitmap {
	if a.IsEmpty() {
		return b
	}
	if b.IsEmpty() {
		return a
	}
	builder := NewBuilder(a.KeyCount() + b.KeyCount())
	builder.reserveData(a.LenInBytes() + b.LenInBytes())
	OrInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndNot returns a minus b as a new Bitmap. Empty operands short-circuit:
// an empty a yields empty, and an empty b yields a unchanged. Disjoint key
// sets also return a unchanged without copying any container payloads.
func AndNot(a, b Bitmap) Bitmap {
	if a.IsEmpty() {
		return Bitmap{}
	}
	if b.IsEmpty() {
		return a
	}
	if !keysOverlap(a, b) {
		return a
	}
	builder := NewBuilder(a.KeyCount())
	builder.reserveData(a.LenInBytes())
	AndNotInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndMany returns the N-way intersection of bitmaps as a new Bitmap. Any
// empty operand short-circuits to an empty Bitmap.
func AndMany(bitmaps ...Bitmap) Bitmap {
	if len(bitmaps) == 0 {
		return Bitmap{}
	}
	for _, bm := range bitmaps {
		if bm.IsEmpty() {
			return Bitmap{}
		}
	}
	capacity := bitmaps[0].KeyCount()
	bytes := bitmaps[0].LenInBytes()
	for _, bm := range bitmaps[1:] {
		if n := bm.KeyCount(); n < capacity {
			capacity = n
		}
		if n := bm.LenInBytes(); n < bytes {
			bytes = n
		}
	}
	builder := NewBuilder(capacity)
	builder.reserveData(bytes)
	AndManyInto(builder, bitmaps...)
	return Bitmap{data: builder.Compact()}
}

// OrMany returns the N-way union of bitmaps as a new Bitmap. Empty operands
// are skipped; when exactly one operand is non-empty it is returned directly
// without building, since Bitmaps are immutable.
func OrMany(bitmaps ...Bitmap) Bitmap {
	var capacity, bytes int
	var sole Bitmap
	live := 0
	for _, bm := range bitmaps {
		if bm.IsEmpty() {
			continue
		}
		sole = bm
		live++
		capacity += bm.KeyCount()
		bytes += bm.LenInBytes()
	}
	switch live {
	case 0:
		return Bitmap{}
	case 1:
		return sole
	}
	builder := NewBuilder(capacity)
	builder.reserveData(bytes)
	OrManyInto(builder, bitmaps...)
	return Bitmap{data: builder.Compact()}
}

// AndInto appends the intersection of a and b to dst. The two key tables
// are merge-walked in order; only matching containers are combined.
func AndInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) && j < len(bEntries) {
		switch {
		case aEntries[i].Key < bEntries[j].Key:
			i++
		case aEntries[i].Key > bEntries[j].Key:
			j++
		default:
			andContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
}

func andContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		intersectArrays(&dst.scratchValues, arrayValuesAt(a), arrayValuesAt(b))
		dst.appendArray(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		filterArrayByBitmap(&dst.scratchValues, arrayValuesAt(a), bitmapWordsAt(b))
		dst.appendArray(key, dst.scratchValues)
	case hb.Type == uint16(ContainerArray):
		filterArrayByBitmap(&dst.scratchValues, arrayValuesAt(b), bitmapWordsAt(a))
		dst.appendArray(key, dst.scratchValues)
	default:
		card := andWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// OrInto appends the union of a and b to dst.
func OrInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) || j < len(bEntries) {
		switch {
		case j >= len(bEntries) || i < len(aEntries) && aEntries[i].Key < bEntries[j].Key:
			copyContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:])
			i++
		case i >= len(aEntries) || aEntries[i].Key > bEntries[j].Key:
			copyContainerInto(dst, bEntries[j].Key, b.data[bEntries[j].Offset:])
			j++
		default:
			orContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
}

func orContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		unionArrays(&dst.scratchValues, arrayValuesAt(a), arrayValuesAt(b))
		dst.emitValues(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		orArrayBitmapInto(dst, key, arrayValuesAt(a), bitmapWordsAt(b), hb.Card)
	case hb.Type == uint16(ContainerArray):
		orArrayBitmapInto(dst, key, arrayValuesAt(b), bitmapWordsAt(a), ha.Card)
	default:
		card := orWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// orArrayBitmapInto unions a sparse array with a dense container. The
// result is dense because the input bitmap already exceeds the array
// threshold.
func orArrayBitmapInto(dst *Builder, key uint64, values []uint16, words []uint64, baseCard uint32) {
	scratch := dst.wordsScratch()
	copy(scratch, words)
	card := baseCard
	for _, value := range values {
		word, bit := value>>6, value&63
		mask := uint64(1) << bit
		if scratch[word]&mask == 0 {
			scratch[word] |= mask
			card++
		}
	}
	dst.appendBitmap(key, scratch, card)
}

// AndNotInto appends a minus b to dst.
func AndNotInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) {
		if j >= len(bEntries) || aEntries[i].Key < bEntries[j].Key {
			copyContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:])
			i++
			continue
		}
		if aEntries[i].Key > bEntries[j].Key {
			j++
			continue
		}
		andNotContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
		i++
		j++
	}
}

func andNotContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		differenceArrays(&dst.scratchValues, arrayValuesAt(a), arrayValuesAt(b))
		dst.appendArray(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		filterArrayNotInBitmap(&dst.scratchValues, arrayValuesAt(a), bitmapWordsAt(b))
		dst.appendArray(key, dst.scratchValues)
	case hb.Type == uint16(ContainerArray):
		card := andNotWordsArray(dst.wordsScratch(), bitmapWordsAt(a), arrayValuesAt(b), ha.Card)
		dst.emitWords(key, card)
	default:
		card := andNotWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// filterArrayByBitmap writes the values present in words to *dst.
func filterArrayByBitmap(dst *[]uint16, values []uint16, words []uint64) {
	out := (*dst)[:0]
	for _, value := range values {
		if bitmapContains(words, value) {
			out = append(out, value)
		}
	}
	*dst = out
}

// filterArrayNotInBitmap writes the values absent from words to *dst.
func filterArrayNotInBitmap(dst *[]uint16, values []uint16, words []uint64) {
	out := (*dst)[:0]
	for _, value := range values {
		if !bitmapContains(words, value) {
			out = append(out, value)
		}
	}
	*dst = out
}

// andNotWordsArray writes a bitmap container minus a sparse array into dst
// and returns its cardinality. The dense popcount is skipped: the result is
// the base cardinality minus the cleared bits that were actually set.
func andNotWordsArray(dst, words []uint64, values []uint16, baseCard uint32) uint32 {
	copy(dst, words)
	cleared := uint32(0)
	for _, value := range values {
		word, bit := value>>6, value&63
		mask := uint64(1) << bit
		if dst[word]&mask != 0 {
			dst[word] &^= mask
			cleared++
		}
	}
	return baseCard - cleared
}

// andWords writes the wordwise intersection into dst, which must hold
// BitmapWords, and returns its cardinality.
func andWords(dst, a, b []uint64) uint32 {
	return bitmapkernel.And(dst, a, b)
}

// orWords writes the wordwise union into dst and returns its cardinality.
func orWords(dst, a, b []uint64) uint32 {
	return bitmapkernel.Or(dst, a, b)
}

// andNotWords writes the wordwise difference into dst and returns its
// cardinality.
func andNotWords(dst, a, b []uint64) uint32 {
	return bitmapkernel.AndNot(dst, a, b)
}

// copyContainerInto appends an exact copy of a container to dst. Source
// containers are canonical serialized bytes, so they are copied verbatim
// instead of decoded and re-encoded.
func copyContainerInto(dst *Builder, key uint64, container []byte) {
	h := containerHeaderAt(container)
	n := int(h.Blocks) * 8
	start := len(dst.data)
	region := dst.extendNoClear(n)
	copy(region, container[:n])
	dst.keys = append(dst.keys, key)
	dst.offsets = append(dst.offsets, uint64(start))
	dst.cardinality += uint64(h.Card)
}

// copyAllInto appends a complete copy of src to dst.
func copyAllInto(dst *Builder, src Bitmap) {
	for _, entry := range src.keyEntries() {
		copyContainerInto(dst, entry.Key, src.data[entry.Offset:])
	}
}
