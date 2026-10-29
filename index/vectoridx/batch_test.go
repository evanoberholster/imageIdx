package vectoridx

import (
	"math/rand"
	"path/filepath"
	"testing"
)

func TestAddFloat32Batch(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(601))
	const n = 130
	ids := make([]NodeID, n)
	vecs := make([][]float32, n)
	for i := range ids {
		ids[i] = NodeID(i + 1)
		vecs[i] = randFloatVec(rng, Dim512)
	}
	if err := idx.AddFloat32Batch(ids, vecs); err != nil {
		t.Fatal(err)
	}
	if got := idx.LiveCount(); got != n {
		t.Fatalf("LiveCount = %d, want %d", got, n)
	}
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := idx.AddFloat32Batch([]NodeID{1}, vecs[:1]); err != ErrDuplicateNodeID {
		t.Fatalf("duplicate batch = %v, want ErrDuplicateNodeID", err)
	}
	if err := idx.AddFloat32Batch([]NodeID{NodeID(n + 1)}, [][]float32{make([]float32, Dim512-1)}); err != ErrInvalidVector {
		t.Fatalf("bad width = %v, want ErrInvalidVector", err)
	}
}

func TestAddFloat32BatchReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.idx")
	idx, err := NewStore(path, testConfigFloat512(), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(603))
	const n = 80
	ids := make([]NodeID, n)
	vecs := make([][]float32, n)
	for i := range ids {
		ids[i] = NodeID(i + 1)
		vecs[i] = randFloatVec(rng, Dim512)
	}
	if err := idx.AddFloat32Batch(ids, vecs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != n {
		t.Fatalf("reopened LiveCount = %d, want %d", got, n)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBatch(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(602))
	const n = 100
	ids := make([]NodeID, n)
	vecs := make([][]float32, n)
	for i := range ids {
		ids[i] = NodeID(i + 1)
		vecs[i] = randFloatVec(rng, Dim512)
	}
	if err := idx.AddFloat32Batch(ids, vecs); err != nil {
		t.Fatal(err)
	}
	if err := idx.DeleteBatch([]NodeID{2, 4, 6, 8}); err != nil {
		t.Fatal(err)
	}
	if got := idx.LiveCount(); got != n-4 {
		t.Fatalf("LiveCount = %d, want %d", got, n-4)
	}
	if err := idx.DeleteBatch([]NodeID{999}); err != ErrNodeNotFound {
		t.Fatalf("unknown delete = %v, want ErrNodeNotFound", err)
	}
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
}
