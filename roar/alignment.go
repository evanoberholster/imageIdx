package roar

import "unsafe"

// requireAligned panics if data is non-empty and its base address is not
// Alignment-byte aligned. Every constructor that produces a fresh serialized
// region and hands it to zero-copy uint64 access must call it.
//
// Go only guarantees single-byte alignment for make([]byte, n) and
// bytes.Clone; in practice the runtime returns 8-byte-aligned pointers on
// 64-bit little-endian targets, but the guarantee is not in the language
// spec. This turns any platform where that assumption breaks into an
// immediate, loud failure instead of silent memory corruption from unaligned
// unsafe reads.
func requireAligned(data []byte) {
	if len(data) == 0 {
		return
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(data)))%Alignment != 0 {
		panic("roar: constructed buffer is not 8-byte aligned")
	}
}
