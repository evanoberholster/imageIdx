package index_test

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
)

// TestConcurrentReadersWriter exercises the documented contract: many
// lock-free readers, one serialized writer. It is intended for -race.
func TestConcurrentReadersWriter(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 1; i <= 64; i++ {
		if err := db.Add(index.Entry{ValueID: uint64(i), Times: []index.TimeValue{{Namespace: 1, Timestamp: uint64(i)}}}); err != nil {
			t.Fatal(err)
		}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeIdx, _ := db.Time(1)
			live, val, _ := db.Liveness(common.EntityImage)
			for !stop.Load() {
				_ = timeIdx.Search(nil, 0, 1000)
				if live != nil {
					_, _ = live.MatchValues(val)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		id := uint64(i%64 + 1)
		if err := db.Add(index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: uint64(i % 1000)}}}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := db.Delete(common.EntityImage, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
}

func TestCorruptSchemaFailsOpen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.bin"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Open(dir); err == nil {
		t.Fatal("opened a corrupt schema")
	}
}

func TestMissingNamespaceFileReported(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Remove the timestamp namespace file; its namespace must become
	// unavailable while healthy namespaces still open.
	if err := os.Remove(filepath.Join(dir, "0001.timeidx")); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h, ok := reopened.HealthOf(1)
	if !ok || h.Available {
		t.Fatalf("namespace 1 health = %+v, %v; want unavailable", h, ok)
	}
	if _, ok := reopened.Time(1); ok {
		t.Fatal("Time accessor resolved an unavailable namespace")
	}
	// A healthy unrelated namespace (bitmap tags) remains available.
	if _, ok := reopened.Bitmap(6); !ok {
		t.Fatal("healthy namespace became unavailable")
	}
}
