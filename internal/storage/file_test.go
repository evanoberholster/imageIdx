package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFullAndAtomicFile(t *testing.T) {
	var output bytes.Buffer
	if err := WriteFull(&output, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if output.String() != "payload" {
		t.Fatalf("output = %q", output.String())
	}
	dir := t.TempDir()
	if err := WriteFileAtomic(dir, "manifest.tmp", "manifest", output.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, output.Bytes()) {
		t.Fatalf("data = %q", data)
	}
}

func TestAlignAndCheckedEnd(t *testing.T) {
	if got := Align(4_097, 4_096); got != 8_192 {
		t.Fatalf("Align = %d", got)
	}
	if end, ok := CheckedEnd(8, 4, 8, 40); !ok || end != 40 {
		t.Fatalf("CheckedEnd = %d, %v", end, ok)
	}
	if _, ok := CheckedEnd(^uint64(0)-1, 2, 8, ^uint64(0)); ok {
		t.Fatal("overflow accepted")
	}
	if got := SaturatingAdd(^uint64(0)-1, 2); got != ^uint64(0) {
		t.Fatalf("SaturatingAdd = %d", got)
	}
}
