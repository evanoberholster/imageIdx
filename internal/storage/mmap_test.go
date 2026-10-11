package storage

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMapFileAndView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.base")
	data := make([]byte, 16)
	binary.LittleEndian.PutUint64(data[0:8], 11)
	binary.LittleEndian.PutUint64(data[8:16], 22)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mapping, err := MapFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := View[uint64](mapping, 0, 2, errCorruptTest, errEndianTest)
	if err != nil {
		t.Fatal(err)
	}
	if values[0] != 11 || values[1] != 22 {
		t.Fatalf("values = %v", values)
	}
	if err = Unmap(mapping); err != nil {
		t.Fatal(err)
	}
}

var (
	errCorruptTest = errors.New("test: corrupt")
	errEndianTest  = errors.New("test: endian")
)

func TestMapFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.base")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := MapFile(path, nil); err != nil || len(data) != 0 {
		t.Fatalf("lenient empty = %v, %v", data, err)
	}
	if _, err := MapFile(path, errCorruptTest); !errors.Is(err, errCorruptTest) {
		t.Fatalf("strict empty err = %v", err)
	}
}

func TestViewRejectsInvalidRanges(t *testing.T) {
	data := make([]byte, 16)
	if _, err := View[uint64](data, 8, 2, errCorruptTest, errEndianTest); !errors.Is(err, errCorruptTest) {
		t.Fatalf("bounds error = %v", err)
	}
	if values, err := View[uint64](data, 0, 0, errCorruptTest, errEndianTest); err != nil || values != nil {
		t.Fatalf("zero count = %v, %v", values, err)
	}
}

func TestMappedSizeRejectsBeforeTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	limit := uint64(^uint(0) >> 1)
	for _, size := range []int64{0, -1, int64(limit + 1)} {
		if _, err := CreateMapped(path, size); !errors.Is(err, ErrMappedSize) {
			t.Fatal(size, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep" {
			t.Fatal("invalid size truncated data", err)
		}
	}
	if limit < uint64(^uint64(0)>>1) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(int64(limit + 1)); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
		if _, err := OpenMapped(path); !errors.Is(err, ErrMappedSize) {
			t.Fatal(err)
		}
		if _, err := MapFile(path, nil); !errors.Is(err, ErrMappedSize) {
			t.Fatal(err)
		}
	}
}
