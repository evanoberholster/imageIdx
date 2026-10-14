package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
)

func TestAtomicBatchScanAndRangeCleanup(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "kv"), Spec{Namespace: 44, Name: "entries"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := Key{Namespace: 44, ID: 2}
	b := db.NewBatch()
	if err := b.Set(key, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted value visible: %v", err)
	}
	if err := b.Set(Key{Namespace: 0, ID: 1}, []byte("invalid")); !errors.Is(err, ErrNamespace) {
		t.Fatalf("invalid namespace: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discarded value visible: %v", err)
	}
	b = db.NewBatch()
	for i := uint64(1); i <= 4; i++ {
		if err := b.Set(Key{Namespace: 44, ID: i}, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	if err := db.ScanFrom(common.Namespace(44), 2, func(k Key, v []byte) (bool, error) {
		ids = append(ids, k.ID)
		if len(v) != 1 || v[0] != byte(k.ID) {
			t.Fatalf("bad value at %d", k.ID)
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[2] != 4 {
		t.Fatalf("scan from 2: %v", ids)
	}
	b = db.NewBatch()
	if err := b.DeleteRange(44, 2, 4); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{2, 3} {
		if _, err := db.Get(Key{Namespace: 44, ID: id}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("id %d survived range delete: %v", id, err)
		}
	}
	if _, err := db.Get(Key{Namespace: 44, ID: 4}); err != nil {
		t.Fatal(err)
	}
}
