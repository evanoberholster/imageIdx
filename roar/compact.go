package roar

import "encoding/binary"

// assemble writes the header, key table, and container payloads into one
// compact little-endian byte region. cardinality is the total number of IDs
// across all containers, stored so readers can query it in O(1).
func assemble(keys []uint64, offsets []uint64, data []byte, cardinality uint64) []byte {
	containerBase := HeaderSize + len(keys)*KeyEntrySize
	out := make([]byte, containerBase, containerBase+len(data))
	requireAligned(out)
	writeHeaderKeyTable(out, keys, offsets, cardinality)
	return append(out, data...)
}

// writeHeaderKeyTable writes the header and key table into the front of out,
// whose length is HeaderSize + len(keys)*KeyEntrySize. Container offsets in
// the key table are absolute (containerBase + relative offset).
func writeHeaderKeyTable(out []byte, keys, offsets []uint64, cardinality uint64) {
	binary.LittleEndian.PutUint32(out[0:4], Magic)
	binary.LittleEndian.PutUint16(out[4:6], Version)
	binary.LittleEndian.PutUint16(out[6:8], FlagCompact|FlagStoredCardinality)
	binary.LittleEndian.PutUint32(out[8:12], HeaderSize)
	binary.LittleEndian.PutUint32(out[12:16], uint32(len(keys)))
	binary.LittleEndian.PutUint64(out[16:24], cardinality)
	containerBase := HeaderSize + len(keys)*KeyEntrySize
	for index := range keys {
		entry := HeaderSize + index*KeyEntrySize
		binary.LittleEndian.PutUint64(out[entry:entry+8], keys[index])
		binary.LittleEndian.PutUint64(out[entry+8:entry+16], uint64(containerBase)+offsets[index])
	}
}

// Compact assembles the accumulated containers into a compact serialized
// bitmap. The builder's contents are unchanged and it may be reused after a
// Reset.
func (b *Builder) Compact() []byte {
	return assemble(b.keys, b.offsets, b.data, b.cardinality)
}

// CompactInto assembles the accumulated containers into reuse's backing array
// when it is large enough, avoiding a fresh allocation and the associated
// zeroing. It falls back to Compact otherwise.
func (b *Builder) CompactInto(reuse []byte) []byte {
	containerBase := HeaderSize + len(b.keys)*KeyEntrySize
	total := containerBase + len(b.data)
	if cap(reuse) >= total {
		out := reuse[:total]
		requireAligned(out)
		writeHeaderKeyTable(out, b.keys, b.offsets, b.cardinality)
		copy(out[containerBase:], b.data)
		return out
	}
	return b.Compact()
}

// finishResult returns the accumulated result. When owned is true, reuse must
// be exclusively owned by the result being built; otherwise a fresh owned
// buffer is returned.
func (b *Builder) finishResult(reuse []byte, owned bool) []byte {
	if owned {
		return b.CompactInto(reuse)
	}
	return b.Compact()
}
