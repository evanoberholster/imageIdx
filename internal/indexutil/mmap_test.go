package indexutil

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMmapFileAndMappedSlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.base")
	data := make([]byte, 16)
	binary.LittleEndian.PutUint64(data[0:8], 11)
	binary.LittleEndian.PutUint64(data[8:16], 22)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mapping, err := MmapFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values, err := MappedSlice[uint64](mapping, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if values[0] != 11 || values[1] != 22 {
		t.Fatalf("values = %v", values)
	}
	if err = UnmapFile(mapping); err != nil {
		t.Fatal(err)
	}
}

func TestMappedSliceRejectsInvalidRanges(t *testing.T) {
	data := make([]byte, 16)
	if _, err := MappedSlice[uint64](data, 8, 2); !errors.Is(err, ErrInvalidMapping) {
		t.Fatalf("bounds error = %v", err)
	}
}
