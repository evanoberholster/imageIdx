package roar

// AndCardinality counts the intersection of a and b without materializing
// it. It allocates nothing.
func AndCardinality(a, b Bitmap) uint64 {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	var total uint64
	for i < len(aEntries) && j < len(bEntries) {
		switch {
		case aEntries[i].Key < bEntries[j].Key:
			i++
		case aEntries[i].Key > bEntries[j].Key:
			j++
		default:
			total += andContainerCardinality(a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
	return total
}

// OrCardinality counts the union of a and b without materializing it. It
// allocates nothing.
func OrCardinality(a, b Bitmap) uint64 {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	var total uint64
	for i < len(aEntries) || j < len(bEntries) {
		switch {
		case j >= len(bEntries) || i < len(aEntries) && aEntries[i].Key < bEntries[j].Key:
			total += uint64(containerHeaderAt(a.data[aEntries[i].Offset:]).Card)
			i++
		case i >= len(aEntries) || aEntries[i].Key > bEntries[j].Key:
			total += uint64(containerHeaderAt(b.data[bEntries[j].Offset:]).Card)
			j++
		default:
			total += orContainerCardinality(a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
	return total
}

// AndNotCardinality counts a minus b without materializing it. It allocates
// nothing.
func AndNotCardinality(a, b Bitmap) uint64 {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	var total uint64
	for i < len(aEntries) {
		if j >= len(bEntries) || aEntries[i].Key < bEntries[j].Key {
			total += uint64(containerHeaderAt(a.data[aEntries[i].Offset:]).Card)
			i++
			continue
		}
		if aEntries[i].Key > bEntries[j].Key {
			j++
			continue
		}
		total += andNotContainerCardinality(a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
		i++
		j++
	}
	return total
}

// Intersects reports whether a and b share any ID, exiting at the first
// matching container or bit. It allocates nothing.
func Intersects(a, b Bitmap) bool {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) && j < len(bEntries) {
		switch {
		case aEntries[i].Key < bEntries[j].Key:
			i++
		case aEntries[i].Key > bEntries[j].Key:
			j++
		default:
			if containersIntersect(a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:]) {
				return true
			}
			i++
			j++
		}
	}
	return false
}

func andContainerCardinality(a, b []byte) uint64 {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		return uint64(intersectArraysCount(arrayValuesAt(a), arrayValuesAt(b)))
	case ha.Type == uint16(ContainerArray):
		return uint64(countArrayInBitmap(arrayValuesAt(a), bitmapWordsAt(b)))
	case hb.Type == uint16(ContainerArray):
		return uint64(countArrayInBitmap(arrayValuesAt(b), bitmapWordsAt(a)))
	default:
		return andWordsCardinality(bitmapWordsAt(a), bitmapWordsAt(b))
	}
}

func orContainerCardinality(a, b []byte) uint64 {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		return uint64(unionArraysCount(arrayValuesAt(a), arrayValuesAt(b)))
	case ha.Type == uint16(ContainerArray):
		return uint64(hb.Card) + uint64(countArrayNotInBitmap(arrayValuesAt(a), bitmapWordsAt(b)))
	case hb.Type == uint16(ContainerArray):
		return uint64(ha.Card) + uint64(countArrayNotInBitmap(arrayValuesAt(b), bitmapWordsAt(a)))
	default:
		return orWordsCardinality(bitmapWordsAt(a), bitmapWordsAt(b))
	}
}

func andNotContainerCardinality(a, b []byte) uint64 {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		return uint64(differenceArraysCount(arrayValuesAt(a), arrayValuesAt(b)))
	case ha.Type == uint16(ContainerArray):
		return uint64(countArrayNotInBitmap(arrayValuesAt(a), bitmapWordsAt(b)))
	case hb.Type == uint16(ContainerArray):
		return andNotWordsCardinalityArray(bitmapWordsAt(a), arrayValuesAt(b))
	default:
		return andNotWordsCardinality(bitmapWordsAt(a), bitmapWordsAt(b))
	}
}

func containersIntersect(a, b []byte) bool {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		return arraysIntersect(arrayValuesAt(a), arrayValuesAt(b))
	case ha.Type == uint16(ContainerArray):
		return arrayIntersectsBitmap(arrayValuesAt(a), bitmapWordsAt(b))
	case hb.Type == uint16(ContainerArray):
		return arrayIntersectsBitmap(arrayValuesAt(b), bitmapWordsAt(a))
	default:
		return wordsIntersect(bitmapWordsAt(a), bitmapWordsAt(b))
	}
}

func countArrayInBitmap(values []uint16, words []uint64) int {
	count := 0
	for _, value := range values {
		if bitmapContains(words, value) {
			count++
		}
	}
	return count
}

func countArrayNotInBitmap(values []uint16, words []uint64) int {
	return len(values) - countArrayInBitmap(values, words)
}

func arraysIntersect(a, b []uint16) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			return true
		}
	}
	return false
}

func arrayIntersectsBitmap(values []uint16, words []uint64) bool {
	for _, value := range values {
		if bitmapContains(words, value) {
			return true
		}
	}
	return false
}

func andNotWordsCardinalityArray(words []uint64, values []uint16) uint64 {
	count := wordsCardinality(words)
	for _, value := range values {
		if bitmapContains(words, value) {
			count--
		}
	}
	return count
}
