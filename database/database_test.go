package database

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

const tagNS schematypes.Namespace = 101

func projection(id uint64, value byte) []byte {
	var b [9]byte
	binary.BigEndian.PutUint64(b[:8], id)
	b[8] = value
	return b[:]
}

func decodeTest(b []byte) (index.Entry, error) {
	if len(b) != 9 {
		return index.Entry{}, ErrCorruptJournal
	}
	return index.Entry{ValueID: binary.BigEndian.Uint64(b[:8]), Bitmaps: []index.BitmapValue{{Namespace: tagNS, Value: bitmapidx.EnumBytesValue(b[8:9])}}}, nil
}

func paths(t testing.TB) (string, string) {
	t.Helper()
	root := t.TempDir()
	data, idxPath := filepath.Join(root, "data"), filepath.Join(root, "index")
	schema := schematypes.NewDefinitions()
	def, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: tagNS, Kind: schematypes.KindBitmapEnum}, "tags", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(def); err != nil {
		t.Fatal(err)
	}
	other, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 102, Kind: schematypes.KindBitmapEnum}, "untouched", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(other); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(idxPath, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	return data, idxPath
}

func openTest(t *testing.T, data, idxPath string) *DB {
	t.Helper()
	db, err := Open(data, idxPath, decodeTest, Options{BatchSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func wait(t *testing.T, db *DB, generation uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.WaitIndexed(ctx, generation); err != nil {
		t.Fatal(err)
	}
	if db.IndexedGeneration() > db.DatabaseGeneration() {
		t.Fatal("indexed beyond committed")
	}
	if db.Index().SourceGeneration() < generation {
		t.Fatal("ack ahead of durable index")
	}
}

func contains(t *testing.T, db *DB, id uint64, value byte, want bool) {
	t.Helper()
	idx, ok := db.Index().Bitmap(tagNS)
	if !ok {
		t.Fatal("missing bitmap")
	}
	got, err := idx.MatchValues(bitmapidx.EnumBytesValue([]byte{value}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	found = got.Contains(id)
	if found != want {
		t.Fatalf("id %d value %c found=%t, want %t", id, value, found, want)
	}
}

func TestCommitReplayAndReopen(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Record: []byte("image"), Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Get(schematypes.EntityImage, 1)
	if err != nil || string(got) != "image" {
		t.Fatalf("canonical record %q: %v", got, err)
	}
	wait(t, db, g)
	contains(t, db, 1, 'a', true)
	g, err = db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Record: []byte("metadata only"), Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	contains(t, db, 1, 'a', true)
	g, err = db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Record: []byte("updated"), Projection: projection(1, 'b')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	contains(t, db, 1, 'a', false)
	contains(t, db, 1, 'b', true)
	g, err = db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if _, err := db.Get(schematypes.EntityImage, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	g, err = db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Projection: projection(1, 'c')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	contains(t, db, 1, 'b', false)
	contains(t, db, 1, 'c', true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idxPath)
	defer db.Close()
	if db.IndexedGeneration() != g {
		t.Fatalf("indexed = %d, want %d", db.IndexedGeneration(), g)
	}
	contains(t, db, 1, 'c', true)
}

func TestFailedCommitDoesNotAdvance(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	_, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 4, Projection: projection(4, 'a')}, Mutation{Entity: schematypes.EntityImage, ValueID: 4, Projection: projection(4, 'b')})
	if !errors.Is(err, ErrInvalidMutation) {
		t.Fatalf("duplicate key: %v", err)
	}
	if db.DatabaseGeneration() != 0 {
		t.Fatal("failed batch advanced generation")
	}
	if _, err := db.Get(schematypes.EntityImage, 4); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed batch visible: %v", err)
	}
	g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 4, Projection: projection(4, 'a')})
	if err != nil || g != 1 {
		t.Fatalf("sequence = %d: %v", g, err)
	}
	wait(t, db, g)
}

func TestConcurrentSequences(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	const n = 24
	results := make(chan uint64, n)
	errorsCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := uint64(1); i <= n; i++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: id, Projection: projection(id, byte(id))})
			if err != nil {
				errorsCh <- err
				return
			}
			results <- g
		}(i)
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	seen := make(map[uint64]bool)
	for g := range results {
		if seen[g] {
			t.Errorf("duplicate sequence %d", g)
		}
		seen[g] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d sequences", len(seen))
	}
	for i := uint64(1); i <= n; i++ {
		if !seen[i] {
			t.Error(fmt.Sprintf("missing sequence %d", i))
		}
	}
	wait(t, db, n)
}

func TestAcknowledgmentCannotExceedRecoverableIndex(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 5, Projection: projection(5, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	batch := db.values.NewBatch()
	if err := putGeneration(batch, 2, g+1); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(data, idxPath, decodeTest, Options{})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unsupported acknowledgment: %v", err)
	}
}

func TestCorruptPendingJournalStopsReplay(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	// Commit a valid mutation without waking the publisher; simulate a
	// committed but damaged journal record at startup.
	db.mu.Lock()
	b := db.values.NewBatch()
	key := store.Key{Namespace: journalNS, ID: 1}
	if err := b.Set(key, []byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := putGeneration(b, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	db.committed = 1
	db.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db.signal()
	if err := db.WaitIndexed(ctx, 1); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("damaged journal: %v", err)
	}
	if db.IndexedGeneration() != 0 {
		t.Fatal("corrupt record acknowledged")
	}
	for _, status := range db.RecoveryStatus() {
		if status.State != RecoveryFailed || !errors.Is(status.Err, ErrCorruptJournal) {
			t.Fatalf("corrupt journal status: %+v", status)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_ = idxPath
}

func TestWaitIndexedCancellation(t *testing.T) {
	data, idxPath := paths(t)
	blocked := make(chan struct{})
	release := make(chan struct{})
	db, err := Open(data, idxPath, decodeTest, Options{boundary: func(at string) {
		if at == "before index apply" {
			close(blocked)
			<-release
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 9, Projection: projection(9, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	<-blocked
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.WaitIndexed(ctx, g); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	close(release)
	wait(t, db, g)
}
