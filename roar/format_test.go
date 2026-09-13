package roar

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
	"unsafe"
)

// TestFormatConstantsAligned locks the structural sizes that guarantee
// 8-byte alignment throughout the serialized layout, plus the exact in-memory
// header layout that the new offsets below depend on.
func TestFormatConstantsAligned(t *testing.T) {
	if HeaderSize%Alignment != 0 {
		t.Fatalf("HeaderSize = %d is not %d-byte aligned", HeaderSize, Alignment)
	}
	if KeyEntrySize%Alignment != 0 {
		t.Fatalf("KeyEntrySize = %d is not %d-byte aligned", KeyEntrySize, Alignment)
	}
	if ContainerHeaderSize%Alignment != 0 {
		t.Fatalf("ContainerHeaderSize = %d is not %d-byte aligned", ContainerHeaderSize, Alignment)
	}
	if BitmapBytes%Alignment != 0 {
		t.Fatalf("BitmapBytes = %d is not %d-byte aligned", BitmapBytes, Alignment)
	}
	if size := unsafe.Sizeof(header{}); size != HeaderSize {
		t.Fatalf("sizeof(header) = %d, want HeaderSize %d", size, HeaderSize)
	}
	var h header
	base := unsafe.Pointer(&h)
	if off := uintptr(unsafe.Pointer(&h.Version)) - uintptr(base); off != 0 {
		t.Fatalf("header.Version offset = %d, want 0", off)
	}
	if off := uintptr(unsafe.Pointer(&h.Flags)) - uintptr(base); off != 2 {
		t.Fatalf("header.Flags offset = %d, want 2", off)
	}
	if off := uintptr(unsafe.Pointer(&h.KeyCount)) - uintptr(base); off != 4 {
		t.Fatalf("header.KeyCount offset = %d, want 4", off)
	}
	if off := uintptr(unsafe.Pointer(&h.Cardinality)) - uintptr(base); off != 8 {
		t.Fatalf("header.Cardinality offset = %d, want 8", off)
	}
}

// TestSerializedAlignment walks every produced serialization and asserts
// that the header, key table, and each container satisfy the alignment
// invariants required by zero-copy uint64 access.
func TestSerializedAlignment(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	datasets := [][]uint64{
		nil,
		{1 << 48},
		{1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 1<<48 + 4},
		{1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 1<<48 + 4, 1<<48 + 5},
		{1<<48 + 1, 1<<48 + 2, 2<<48 + 100, 3<<48 + 65535},
	}
	for round := 0; round < 20; round++ {
		datasets = append(datasets, randomSortedIDs(rng, 64))
		datasets = append(datasets, denseSortedIDs(rng, 8))
	}

	views := make([]Bitmap, len(datasets))
	for index, ids := range datasets {
		bitmap, err := FromSorted(ids)
		if err != nil {
			t.Fatalf("FromSorted(%d): %v", index, err)
		}
		assertAligned(t, bitmap.Bytes())
		views[index] = bitmap
	}

	builder := NewBuilder(0)
	results := [][]byte{}
	for left := 0; left < len(views); left++ {
		for right := left; right < len(views); right++ {
			builder.Reset()
			AndInto(builder, views[left], views[right])
			results = append(results, builder.Compact())
			builder.Reset()
			OrInto(builder, views[left], views[right])
			results = append(results, builder.Compact())
			builder.Reset()
			AndNotInto(builder, views[left], views[right])
			results = append(results, builder.Compact())
		}
	}
	for round := 0; round < 10; round++ {
		group := []Bitmap{
			views[rng.IntN(len(views))],
			views[rng.IntN(len(views))],
			views[rng.IntN(len(views))],
			views[rng.IntN(len(views))],
		}
		builder.Reset()
		AndManyInto(builder, group...)
		results = append(results, builder.Compact())
	}

	for index, data := range results {
		if data == nil {
			continue
		}
		assertAligned(t, data)
		if _, err := Open(data); err != nil {
			t.Fatalf("result %d failed validation: %v", index, err)
		}
	}
}

func assertAligned(t *testing.T, data []byte) {
	t.Helper()
	if len(data)%Alignment != 0 {
		t.Fatalf("serialized size %d is not %d-byte aligned", len(data), Alignment)
	}
	headerBytes := uint64(HeaderSize)
	keyCount := binary.LittleEndian.Uint32(data[4:8])
	if headerBytes%Alignment != 0 {
		t.Fatalf("key table start %d is not %d-byte aligned", headerBytes, Alignment)
	}
	tableEnd := uint64(headerBytes) + uint64(keyCount)*KeyEntrySize
	for index := uint32(0); index < keyCount; index++ {
		entry := uint64(headerBytes) + uint64(index)*KeyEntrySize
		offset := binary.LittleEndian.Uint64(data[entry+8 : entry+16])
		if offset%Alignment != 0 {
			t.Fatalf("container offset %d is not %d-byte aligned", offset, Alignment)
		}
		if offset < tableEnd {
			t.Fatalf("container offset %d overlaps key table ending at %d", offset, tableEnd)
		}
		blocks := uint64(binary.LittleEndian.Uint16(data[offset : offset+2]))
		end := offset + blocks*8
		if end%Alignment != 0 || end > uint64(len(data)) {
			t.Fatalf("container at %d ends at %d: unaligned or out of bounds", offset, end)
		}
	}
}

func TestGoldenBytes(t *testing.T) {
	empty, err := FromSorted(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantEmpty := []byte{
		0x01, 0x00, // version 1
		0x03, 0x00, // FlagCompact | FlagStoredCardinality
		0x00, 0x00, 0x00, 0x00, // KeyCount 0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Cardinality 0
	}
	if got := empty.Bytes(); string(got) != string(wantEmpty) {
		t.Fatalf("empty bitmap = % x, want % x", got, wantEmpty)
	}

	one, err := FromSorted([]uint64{1<<48 + 5})
	if err != nil {
		t.Fatal(err)
	}
	wantOne := []byte{
		0x01, 0x00,
		0x03, 0x00,
		0x01, 0x00, 0x00, 0x00, // KeyCount 1
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Cardinality 1
		// key entry: key = 1<<48, offset = 32
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
		0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// container: blocks 2, array, card 1
		0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		// values: 5, padding
		0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if got := one.Bytes(); string(got) != string(wantOne) {
		t.Fatalf("one-ID bitmap = % x, want % x", got, wantOne)
	}
}

// TestRejectsPreSealLayouts locks the seal promise: bytes written by
// pre-seal layouts must fail closed, never parse. The development magic word
// lands on the version tag and mismatches; the short-lived 24-byte header
// parses its headerBytes word as key count 24 and runs out of bounds.
func TestRejectsPreSealLayouts(t *testing.T) {
	withMagic := []byte{
		0x52, 0x4F, 0x41, 0x52, // "ROAR" over the version tag
		0x03, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if err := Validate(withMagic); err != ErrUnsupportedVersion {
		t.Fatalf("magic-prefixed bytes error = %v, want %v", err, ErrUnsupportedVersion)
	}

	twentyFour := []byte{
		0x01, 0x00,
		0x03, 0x00,
		0x18, 0x00, 0x00, 0x00, // old headerBytes word reads as keyCount 24
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
		0x28, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if err := Validate(twentyFour); err == nil {
		t.Fatal("24-byte-header bytes validated under the 16-byte layout")
	}
}
