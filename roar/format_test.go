package roar

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// TestFormatConstantsAligned locks the structural sizes that guarantee
// 8-byte alignment throughout the serialized layout.
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

	views := make([]View, len(datasets))
	for index, ids := range datasets {
		data, err := FromSorted(ids)
		if err != nil {
			t.Fatalf("FromSorted(%d): %v", index, err)
		}
		assertAligned(t, data)
		views[index] = OpenTrusted(data)
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
		group := []View{
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
	headerBytes := binary.LittleEndian.Uint32(data[8:12])
	keyCount := binary.LittleEndian.Uint32(data[12:16])
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
		0x52, 0x41, 0x4F, 0x52, // "ROAR" little-endian
		0x01, 0x00, // version 1
		0x01, 0x00, // FlagCompact
		0x10, 0x00, 0x00, 0x00, // HeaderBytes 16
		0x00, 0x00, 0x00, 0x00, // KeyCount 0
	}
	if string(empty) != string(wantEmpty) {
		t.Fatalf("empty bitmap = % x, want % x", empty, wantEmpty)
	}

	one, err := FromSorted([]uint64{1<<48 + 5})
	if err != nil {
		t.Fatal(err)
	}
	wantOne := []byte{
		0x52, 0x41, 0x4F, 0x52,
		0x01, 0x00,
		0x01, 0x00,
		0x10, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, // KeyCount 1
		// key entry: key = 1<<48, offset = 32
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
		0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// container: blocks 2, array, card 1
		0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		// values: 5, padding
		0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if string(one) != string(wantOne) {
		t.Fatalf("one-ID bitmap = % x, want % x", one, wantOne)
	}
}
