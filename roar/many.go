package roar

import "math/bits"

// maxStackViews bounds the stack-scratch path for N-way operations.
const maxStackViews = 8

// AndManyInto appends the N-way intersection of views to dst without
// materializing intermediate bitmaps. Common keys are located with one
// merge pass; each matching container set is combined using the smallest
// container as the driver.
func AndManyInto(dst *Builder, views ...Bitmap) {
	if len(views) == 0 {
		return
	}
	if len(views) == 1 {
		copyAllInto(dst, views[0])
		return
	}
	entries := make([][]keyEntry, len(views))
	for index, view := range views {
		entries[index] = view.keyEntries()
	}
	positions := make([]int, len(views))
	containers := make([][]byte, len(views))
	for {
		minKey, found := minEntryKey(entries, positions)
		if !found {
			return
		}
		allMatch := true
		for index := range entries {
			position := positions[index]
			if position < len(entries[index]) && entries[index][position].Key == minKey {
				containers[index] = views[index].data[entries[index][position].Offset:]
			} else {
				containers[index] = nil
				allMatch = false
			}
		}
		if allMatch {
			andManyContainerInto(dst, minKey, containers)
		}
		for index := range entries {
			position := positions[index]
			if position < len(entries[index]) && entries[index][position].Key <= minKey {
				positions[index] = position + 1
			}
		}
	}
}

func minEntryKey(entries [][]keyEntry, positions []int) (uint64, bool) {
	var minKey uint64
	found := false
	for index, list := range entries {
		if positions[index] < len(list) {
			key := list[positions[index]].Key
			if !found || key < minKey {
				minKey = key
				found = true
			}
		}
	}
	return minKey, found
}

func andManyContainerInto(dst *Builder, key uint64, containers [][]byte) {
	allBitmap := true
	driver := 0
	driverCard := ^uint32(0)
	for index, container := range containers {
		card := containerHeaderAt(container).Card
		if ContainerType(containerHeaderAt(container).Type) != ContainerBitmap {
			allBitmap = false
		}
		if card < driverCard {
			driver = index
			driverCard = card
		}
	}

	if allBitmap {
		words := bitmapWordsAt(containers[0])
		copy(dst.scratchWords, words)
		for _, container := range containers[1:] {
			other := bitmapWordsAt(container)
			for index := 0; index < BitmapWords; index++ {
				dst.scratchWords[index] &= other[index]
			}
		}
		dst.emitWords(key, uint32(wordsCardinality(dst.scratchWords)))
		return
	}

	dst.scratchValues = dst.scratchValues[:0]
	clear(dst.scratchWords)
	survivors := 0
	driverContainer := containers[driver]
	if ContainerType(containerHeaderAt(driverContainer).Type) == ContainerArray {
		for _, value := range arrayValuesAt(driverContainer) {
			if valuePresentInAll(value, driver, containers) {
				dst.scratchValues = append(dst.scratchValues, value)
				survivors++
			}
		}
		dst.appendArray(key, dst.scratchValues)
		return
	}
	for wordIndex, word := range bitmapWordsAt(driverContainer) {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			value := uint16(wordIndex<<6 | bit)
			if valuePresentInAll(value, driver, containers) {
				dst.scratchValues = append(dst.scratchValues, value)
				dst.scratchWords[wordIndex] |= uint64(1) << bit
				survivors++
			}
			word &= word - 1
		}
	}
	if survivors == 0 {
		return
	}
	if survivors <= ArrayToBitmapThreshold {
		dst.appendArray(key, dst.scratchValues)
		return
	}
	dst.appendBitmap(key, dst.scratchWords, uint32(survivors))
}

func valuePresentInAll(value uint16, driver int, containers [][]byte) bool {
	for index, container := range containers {
		if index == driver {
			continue
		}
		if !containsContainer(container, value) {
			return false
		}
	}
	return true
}

// AndManyCardinality counts the N-way intersection of views without
// materializing it. It allocates nothing for up to maxStackViews inputs.
func AndManyCardinality(views ...Bitmap) uint64 {
	if len(views) < 2 {
		if len(views) == 1 {
			return views[0].Cardinality()
		}
		return 0
	}
	var stackEntries [maxStackViews][]keyEntry
	var stackPositions [maxStackViews]int
	var stackContainers [maxStackViews][]byte
	entries := stackEntries[:0]
	positions := stackPositions[:0]
	containers := stackContainers[:0]
	if len(views) > maxStackViews {
		entries = make([][]keyEntry, len(views))
		positions = make([]int, len(views))
		containers = make([][]byte, len(views))
	}
	entries = entries[:len(views)]
	positions = positions[:len(views)]
	containers = containers[:len(views)]
	for index, view := range views {
		entries[index] = view.keyEntries()
	}

	var total uint64
	for {
		minKey, found := minEntryKey(entries, positions)
		if !found {
			return total
		}
		allMatch := true
		for index := range entries {
			position := positions[index]
			if position < len(entries[index]) && entries[index][position].Key == minKey {
				containers[index] = views[index].data[entries[index][position].Offset:]
			} else {
				containers[index] = nil
				allMatch = false
			}
		}
		if allMatch {
			total += andManyContainerCardinality(containers)
		}
		for index := range entries {
			position := positions[index]
			if position < len(entries[index]) && entries[index][position].Key <= minKey {
				positions[index] = position + 1
			}
		}
	}
}

func andManyContainerCardinality(containers [][]byte) uint64 {
	allBitmap := true
	driver := 0
	driverCard := ^uint32(0)
	for index, container := range containers {
		card := containerHeaderAt(container).Card
		if ContainerType(containerHeaderAt(container).Type) != ContainerBitmap {
			allBitmap = false
		}
		if card < driverCard {
			driver = index
			driverCard = card
		}
	}

	if allBitmap {
		var count uint64
		for wordIndex := 0; wordIndex < BitmapWords; wordIndex++ {
			word := bitmapWordsAt(containers[0])[wordIndex]
			for _, container := range containers[1:] {
				word &= bitmapWordsAt(container)[wordIndex]
			}
			count += uint64(bits.OnesCount64(word))
		}
		return count
	}

	var count uint64
	driverContainer := containers[driver]
	if ContainerType(containerHeaderAt(driverContainer).Type) == ContainerArray {
		for _, value := range arrayValuesAt(driverContainer) {
			if valuePresentInAll(value, driver, containers) {
				count++
			}
		}
		return count
	}
	for wordIndex, word := range bitmapWordsAt(driverContainer) {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			value := uint16(wordIndex<<6 | bit)
			if valuePresentInAll(value, driver, containers) {
				count++
			}
			word &= word - 1
		}
	}
	return count
}
