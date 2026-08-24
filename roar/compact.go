package roar

import "encoding/binary"

// assemble writes the header, key table, and container payloads into one
// compact little-endian byte region.
func assemble(keys []uint64, offsets []uint64, data []byte) []byte {
	containerBase := HeaderSize + len(keys)*KeyEntrySize
	out := make([]byte, containerBase+len(data))
	binary.LittleEndian.PutUint32(out[0:4], Magic)
	binary.LittleEndian.PutUint16(out[4:6], Version)
	binary.LittleEndian.PutUint16(out[6:8], FlagCompact)
	binary.LittleEndian.PutUint32(out[8:12], HeaderSize)
	binary.LittleEndian.PutUint32(out[12:16], uint32(len(keys)))
	for index := range keys {
		entry := HeaderSize + index*KeyEntrySize
		binary.LittleEndian.PutUint64(out[entry:entry+8], keys[index])
		binary.LittleEndian.PutUint64(out[entry+8:entry+16], uint64(containerBase)+offsets[index])
	}
	copy(out[containerBase:], data)
	return out
}

// Compact assembles the accumulated containers into a compact serialized
// bitmap. The builder's contents are unchanged and it may be reused after a
// Reset.
func (b *Builder) Compact() []byte {
	return assemble(b.keys, b.offsets, b.data)
}
