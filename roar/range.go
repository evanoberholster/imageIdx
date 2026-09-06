package roar

// RemoveRange returns a new Bitmap with all IDs in the half-open interval
// [lo, hi) removed. If hi <= lo the receiver is returned unchanged. The
// receiver is never modified.
func (b Bitmap) RemoveRange(lo, hi uint64) Bitmap {
	if hi <= lo {
		return b
	}
	builder := NewBuilder(b.KeyCount())
	removeRangeInto(builder, b, lo, hi)
	return Bitmap{data: builder.Compact()}
}

// removeRangeInto appends src minus the IDs in [lo, hi) to dst.
func removeRangeInto(dst *Builder, src Bitmap, lo, hi uint64) {
	loKey := highKey(lo)
	hiKey := highKey(hi)
	loVal := int(lowValue(lo))
	hiVal := int(lowValue(hi))

	for _, entry := range src.keyEntries() {
		key := entry.Key
		container := src.data[entry.Offset:]
		switch {
		case key < loKey || key > hiKey:
			copyContainerInto(dst, key, container)
		case key == loKey && key == hiKey:
			removeValuesInto(dst, key, container, loVal, hiVal)
		case key == loKey:
			removeValuesInto(dst, key, container, loVal, 1<<16)
		case key == hiKey:
			removeValuesInto(dst, key, container, 0, hiVal)
		default:
			// loKey < key < hiKey: the whole container is removed.
		}
	}
}

// removeValuesInto appends container minus the values in the half-open range
// [lo, hi) to dst. hi may equal 1<<16 to mean "through the end of the
// container".
func removeValuesInto(dst *Builder, key uint64, container []byte, lo, hi int) {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		values := arrayValuesAt(container)
		dst.scratchValues = dst.scratchValues[:0]
		for _, value := range values {
			v := int(value)
			if v >= lo && v < hi {
				continue
			}
			dst.scratchValues = append(dst.scratchValues, value)
		}
		dst.appendArray(key, dst.scratchValues)
		return
	}
	words := bitmapWordsAt(container)
	scratch := dst.wordsScratch()
	copy(scratch, words)
	for v := lo; v < hi; v++ {
		scratch[v>>6] &^= uint64(1) << (v & 63)
	}
	dst.emitWords(key, uint32(wordsCardinality(scratch)))
}
