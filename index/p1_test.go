package index_test

import (
	"slices"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func assertStoresEqual(t *testing.T, a, b *index.Store) {
	t.Helper()
	at, _ := a.Time(1)
	bt, _ := b.Time(1)
	if at == nil || bt == nil {
		t.Fatal("time namespace unavailable")
	}
	max := ^uint64(0)
	if got, want := at.Search(nil, 0, max), bt.Search(nil, 0, max); !slices.Equal(got, want) {
		t.Fatalf("time search mismatch: batch=%v sequential=%v", got, want)
	}
	atag, _ := a.Bitmap(6)
	btag, _ := b.Bitmap(6)
	av, _ := atag.MatchValues(bitmapidx.EnumStringValue("t"))
	bv, _ := btag.MatchValues(bitmapidx.EnumStringValue("t"))
	if got, want := av.AppendTo(nil), bv.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("tag mismatch: batch=%v sequential=%v", got, want)
	}
	al, av2, _ := a.Liveness(common.EntityImage)
	bl, bv2, _ := b.Liveness(common.EntityImage)
	am, _ := al.MatchValues(av2)
	bm, _ := bl.MatchValues(bv2)
	if got, want := am.AppendTo(nil), bm.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("liveness mismatch: batch=%v sequential=%v", got, want)
	}
}

// TestBatchMatchesSequential verifies grouped engine batching produces exactly
// the same observable state as applying each operation individually.
func TestBatchMatchesSequential(t *testing.T) {
	a, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var (
		adds    []index.Entry
		changes []index.Change
		dels    []index.DeleteOp
	)
	for id := uint64(1); id <= 64; id++ {
		adds = append(adds, index.Entry{
			ValueID: id,
			Times:   []index.TimeValue{{Namespace: 1, Timestamp: id}},
			Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("t")}},
		})
	}
	for id := uint64(2); id <= 64; id += 2 {
		changes = append(changes, index.Change{
			Before: index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: id}}},
			After:  index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: id + 100}}},
		})
	}
	for id := uint64(7); id <= 63; id += 7 {
		dels = append(dels, index.DeleteOp{Entity: common.EntityImage, ID: id})
	}

	// The documented batch contract applies deletes, then changes, then adds.
	// Mirror that order for the sequential reference.
	batch := a.NewBatch(0)
	for _, d := range dels {
		if err := batch.Delete(d.Entity, d.ID); err != nil {
			t.Fatal(err)
		}
		if err := b.Delete(d.Entity, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range changes {
		if err := batch.Update(c); err != nil {
			t.Fatal(err)
		}
		if err := b.Update(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range adds {
		if err := batch.Add(e); err != nil {
			t.Fatal(err)
		}
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Apply(batch); err != nil {
		t.Fatal(err)
	}
	assertStoresEqual(t, a, b)
}

// TestSchemaImmutable verifies a Store never shares mutable schema state with
// the caller and cannot gain namespaces at runtime.
func TestSchemaImmutable(t *testing.T) {
	schema := testSchema(t)
	db, err := index.Create(t.TempDir(), index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Mutating the caller schema after Create must not affect the Store.
	if err := schema.Register(common.NamespaceDef{Namespace: 20, Name: "late", Kind: common.KindTimestamp, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Time(20); ok {
		t.Fatal("runtime namespace became visible")
	}

	// A returned NamespaceDef is a deep copy.
	original, ok := db.NamespaceDef(6)
	if !ok || len(original.Config) == 0 {
		t.Fatalf("namespace 6 def = %+v", original)
	}
	original.Config[0] ^= 0xFF
	again, _ := db.NamespaceDef(6)
	if again.Config[0] == original.Config[0] {
		t.Fatal("mutating a returned NamespaceDef changed stored configuration")
	}

	// Schema() is a defensive copy.
	clone := db.Schema()
	if err := clone.Register(common.NamespaceDef{Namespace: 21, Name: "clone", Kind: common.KindTimestamp, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.NamespaceDef(21); ok {
		t.Fatal("mutating Schema() copy changed the Store")
	}
}

// TestLifecycleConcurrentWithMutations runs checkpoint/sync against a mutating
// writer; it is intended for -race.
func TestLifecycleConcurrentWithMutations(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = db.Checkpoint()
				_ = db.Sync()
			}
		}
	}()
	for i := 0; i < 100; i++ {
		id := uint64(i%32 + 1)
		if err := db.Add(index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: uint64(i)}}}); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := db.Delete(common.EntityImage, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
}
