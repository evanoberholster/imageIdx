package roar

// applyInto appends (base ∪ add) ∖ remove to dst in one pass over the three
// key tables, so the intermediate union is never materialized as a separate
// bitmap. base and add may be empty; remove may be empty.
func applyInto(dst *Builder, base, add, remove Bitmap) {
	baseEntries := base.keyEntries()
	addEntries := add.keyEntries()
	removeEntries := remove.keyEntries()

	i, j, k := 0, 0, 0
	for i < len(baseEntries) || j < len(addEntries) {
		var key uint64
		var baseC, addC []byte
		switch {
		case j >= len(addEntries) || i < len(baseEntries) && baseEntries[i].Key < addEntries[j].Key:
			key = baseEntries[i].Key
			baseC = base.data[baseEntries[i].Offset:]
			i++
		case i >= len(baseEntries) || baseEntries[i].Key > addEntries[j].Key:
			key = addEntries[j].Key
			addC = add.data[addEntries[j].Offset:]
			j++
		default:
			key = baseEntries[i].Key
			baseC = base.data[baseEntries[i].Offset:]
			addC = add.data[addEntries[j].Offset:]
			i++
			j++
		}

		for k < len(removeEntries) && removeEntries[k].Key < key {
			k++
		}
		var removeC []byte
		if k < len(removeEntries) && removeEntries[k].Key == key {
			removeC = remove.data[removeEntries[k].Offset:]
			k++
		}

		switch {
		case removeC == nil && baseC != nil && addC != nil:
			orContainerInto(dst, key, baseC, addC)
		case removeC == nil && baseC != nil:
			copyContainerInto(dst, key, baseC)
		case removeC == nil && addC != nil:
			copyContainerInto(dst, key, addC)
		default:
			combineThreeInto(dst, key, baseC, addC, removeC)
		}
	}
}

// combineThreeInto appends (base ∪ add) ∖ remove to dst for one key. The
// union is computed as a dense word buffer in dst.scratchWords, so no
// intermediate container is allocated. base and add may be nil; remove must
// be non-nil.
func combineThreeInto(dst *Builder, key uint64, base, add, remove []byte) {
	words := dst.wordsScratch()
	clear(words)
	if base != nil {
		setContainerBits(words, base)
	}
	if add != nil {
		setContainerBits(words, add)
	}
	header := containerHeaderAt(remove)
	if ContainerType(header.Type) == ContainerArray {
		for _, value := range arrayValuesAt(remove) {
			words[value>>6] &^= uint64(1) << (value & 63)
		}
	} else {
		removeWords := bitmapWordsAt(remove)
		for index := range removeWords {
			words[index] &^= removeWords[index]
		}
	}
	dst.emitWords(key, uint32(wordsCardinality(words)))
}

// setContainerBits ORs the set bits of a container into words.
func setContainerBits(words []uint64, container []byte) {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		for _, value := range arrayValuesAt(container) {
			words[value>>6] |= uint64(1) << (value & 63)
		}
		return
	}
	src := bitmapWordsAt(container)
	for index := range src {
		words[index] |= src[index]
	}
}
