package roar

import (
	"runtime"
	"unsafe"
)

// containerHeaderAt returns a zero-copy view of the container header.
// Containers begin at 8-byte aligned offsets within the byte region.
func containerHeaderAt(container []byte) *containerHeader {
	h := (*containerHeader)(unsafe.Pointer(unsafe.SliceData(container)))
	runtime.KeepAlive(container)
	return h
}

// arrayValuesAt returns a zero-copy view of an array container's sorted
// uint16 payload.
func arrayValuesAt(container []byte) []uint16 {
	h := containerHeaderAt(container)
	values := unsafe.Slice(
		(*uint16)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
		int(h.Card),
	)
	runtime.KeepAlive(container)
	return values
}

// bitmapWordsAt returns a zero-copy view of a bitmap container's 1,024
// uint64 words.
func bitmapWordsAt(container []byte) []uint64 {
	words := unsafe.Slice(
		(*uint64)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
		BitmapWords,
	)
	runtime.KeepAlive(container)
	return words
}

// containsContainer reports whether value x is present in a container.
func containsContainer(container []byte, x uint16) bool {
	h := containerHeaderAt(container)
	switch ContainerType(h.Type) {
	case ContainerArray:
		return arrayContains(arrayValuesAt(container), x)
	case ContainerBitmap:
		return bitmapContains(bitmapWordsAt(container), x)
	default:
		return false
	}
}
