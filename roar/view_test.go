package roar

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestViewBasics(t *testing.T) {
	bitmap, err := FromSorted([]uint64{1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 2<<48 + 9})
	if err != nil {
		t.Fatal(err)
	}
	view, err := Open(bitmap.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if view.Cardinality() != 4 {
		t.Fatalf("Cardinality = %d", view.Cardinality())
	}
	if view.KeyCount() != 2 {
		t.Fatalf("KeyCount = %d", view.KeyCount())
	}
	if view.IsEmpty() {
		t.Fatal("IsEmpty = true for populated bitmap")
	}

	empty, err := FromSorted(nil)
	if err != nil {
		t.Fatal(err)
	}
	view = OpenTrusted(empty.Bytes())
	if !view.IsEmpty() || view.Cardinality() != 0 || view.KeyCount() != 0 {
		t.Fatal("empty bitmap invariants violated")
	}
}

func TestOpenValidation(t *testing.T) {
	built, err := FromSorted([]uint64{1<<48 + 8, 1<<48 + 9, 2<<48 + 2})
	if err != nil {
		t.Fatal(err)
	}
	valid := built.Bytes()
	if _, err := Open(valid); err != nil {
		t.Fatalf("valid bitmap rejected: %v", err)
	}

	cases := map[string]func() []byte{
		"truncated header": func() []byte {
			return valid[:10]
		},
		"bad magic": func() []byte {
			corrupt := slices.Clone(valid)
			binary.LittleEndian.PutUint32(corrupt[0:4], 0xdeadbeef)
			return corrupt
		},
		"bad version": func() []byte {
			corrupt := slices.Clone(valid)
			binary.LittleEndian.PutUint16(corrupt[4:6], 9)
			return corrupt
		},
		"misaligned offset": func() []byte {
			corrupt := slices.Clone(valid)
			entry := binary.LittleEndian.Uint32(corrupt[8:12])
			binary.LittleEndian.PutUint64(corrupt[entry+8:], 9)
			return corrupt
		},
		"out of bounds offset": func() []byte {
			corrupt := slices.Clone(valid)
			entry := binary.LittleEndian.Uint32(corrupt[8:12])
			binary.LittleEndian.PutUint64(corrupt[entry+8:], uint64(len(corrupt)))
			return corrupt
		},
		"unsorted keys": func() []byte {
			corrupt := slices.Clone(valid)
			entry := binary.LittleEndian.Uint32(corrupt[8:12])
			binary.LittleEndian.PutUint64(corrupt[entry:], 3<<48)
			binary.LittleEndian.PutUint64(corrupt[entry+16:], 2<<48)
			return corrupt
		},
		"uncleared low key bits": func() []byte {
			corrupt := slices.Clone(valid)
			entry := binary.LittleEndian.Uint32(corrupt[8:12])
			binary.LittleEndian.PutUint64(corrupt[entry:], 1<<48+3)
			return corrupt
		},
		"bad container type": func() []byte {
			corrupt := slices.Clone(valid)
			offset := binary.LittleEndian.Uint64(corrupt[32:40])
			binary.LittleEndian.PutUint16(corrupt[offset+2:], 7)
			return corrupt
		},
		"wrong array order": func() []byte {
			corrupt := slices.Clone(valid)
			offset := binary.LittleEndian.Uint64(corrupt[32:40])
			binary.LittleEndian.PutUint16(corrupt[offset+8:], 9)
			binary.LittleEndian.PutUint16(corrupt[offset+10:], 8)
			return corrupt
		},
		"truncated container": func() []byte {
			return valid[:len(valid)-4]
		},
		"key table out of bounds": func() []byte {
			corrupt := slices.Clone(valid)
			binary.LittleEndian.PutUint32(corrupt[12:16], 1000)
			return corrupt
		},
	}
	for name, corrupt := range cases {
		if _, err := Open(corrupt()); err == nil {
			t.Errorf("%s: accepted corrupt data", name)
		}
	}
}

func TestValidateRejectsOverflowingContainerOffset(t *testing.T) {
	built, err := FromSorted([]uint64{1})
	if err != nil {
		t.Fatal(err)
	}
	data := built.Bytes()
	entry := binary.LittleEndian.Uint32(data[8:12])
	binary.LittleEndian.PutUint64(data[entry+8:], ^uint64(0)-7)

	if err := Validate(data); err != ErrInvalidBitmap {
		t.Fatalf("Validate error = %v, want %v", err, ErrInvalidBitmap)
	}
}

func TestValidateRejectsReservedImageID(t *testing.T) {
	denseIDs := make([]uint64, ArrayToBitmapThreshold+1)
	for index := range denseIDs {
		denseIDs[index] = uint64(index + 1)
	}

	tests := []struct {
		name   string
		ids    []uint64
		mutate func([]byte, uint64)
	}{
		{
			name: "array",
			ids:  []uint64{1},
			mutate: func(data []byte, offset uint64) {
				binary.LittleEndian.PutUint16(data[offset+ContainerHeaderSize:], 0)
			},
		},
		{
			name: "bitmap",
			ids:  denseIDs,
			mutate: func(data []byte, offset uint64) {
				wordOffset := offset + ContainerHeaderSize
				word := binary.LittleEndian.Uint64(data[wordOffset:])
				word = word&^uint64(1<<1) | 1
				binary.LittleEndian.PutUint64(data[wordOffset:], word)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			built, err := FromSorted(test.ids)
			if err != nil {
				t.Fatal(err)
			}
			data := built.Bytes()
			offset := binary.LittleEndian.Uint64(data[32:40])
			test.mutate(data, offset)

			if err := Validate(data); err != ErrInvalidImageID {
				t.Fatalf("Validate error = %v, want %v", err, ErrInvalidImageID)
			}
		})
	}
}
