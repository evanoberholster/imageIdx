package vectoridx

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

// cosineSnapshotMatchesKernel verifies that the cached cosine norms and the
// norm-aware snapshot helpers agree with the public scalar kernels.
func cosineSnapshotMatchesKernel(t *testing.T, idx *VectorIdx, query []float32) {
	t.Helper()
	token := idx.readers.Acquire()
	snap := idx.view.Load()
	defer idx.readers.Release(token)

	if len(snap.normChunks) != snap.vectorChunkCount() {
		t.Fatalf("norm chunks = %d, vector chunks = %d", len(snap.normChunks), snap.vectorChunkCount())
	}
	rawQ := encodeRawFloat32(nil, query)
	q := bytesAsFloat32(rawQ, len(query))
	qNorm := snap.rawNorm(rawQ)
	if want := normFloat32(q); math.Abs(qNorm-want) > 1e-5 {
		t.Fatalf("query norm = %v, want %v", qNorm, want)
	}
	for ord := range snap.ids {
		v := bytesAsFloat32(snap.vectorAt(ord), snap.dims)
		want := CosineFloat32(q, v)
		got := snap.distQuery(rawQ, qNorm, ord)
		if math.Abs(got-want) > 1e-5 {
			t.Fatalf("ord %d: distQuery = %v, want %v", ord, got, want)
		}
		if wantNorm := normFloat32(v); math.Abs(snap.normAt(ord)-wantNorm) > 1e-5 {
			t.Fatalf("ord %d: cached norm = %v, want %v", ord, snap.normAt(ord), wantNorm)
		}
	}
	if len(snap.ids) > 1 {
		for a := range snap.ids {
			b := (a*7 + 3) % len(snap.ids)
			va := bytesAsFloat32(snap.vectorAt(a), snap.dims)
			vb := bytesAsFloat32(snap.vectorAt(b), snap.dims)
			want := CosineFloat32(va, vb)
			got := snap.distOrds(a, b)
			if math.Abs(got-want) > 1e-5 {
				t.Fatalf("distOrds(%d,%d) = %v, want %v", a, b, got, want)
			}
		}
	}
}

func TestCosineNormCacheLifecycle(t *testing.T) {
	idx, err := NewMemory(testConfigCosine512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(201))
	vecs := make([][]float32, 24)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	cosineSnapshotMatchesKernel(t, idx, vecs[0])

	// Delete + re-add is an in-place vector replacement; the cached norm and
	// the graph must both see the new vector.
	if err := idx.Delete(5); err != nil {
		t.Fatal(err)
	}
	vecs[4] = randFloatVec(rng, Dim512)
	if err := idx.AddFloat32(5, vecs[4]); err != nil {
		t.Fatal(err)
	}
	cosineSnapshotMatchesKernel(t, idx, vecs[0])

	if _, err := idx.Compact(); err != nil {
		t.Fatal(err)
	}
	cosineSnapshotMatchesKernel(t, idx, vecs[0])
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestCosineNormCacheReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cosine.idx")
	idx, err := NewStore(path, testConfigCosine512(), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(202))
	vecs := make([][]float32, 20)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	query := vecs[0]
	before, err := idx.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Vacuum materializes the vectors in a base generation, so reopen exercises
	// norm rebuilding in loadRegions rather than delta/WAL replay.
	if err := idx.Vacuum(); err != nil {
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
	cosineSnapshotMatchesKernel(t, reopened, query)
	after, err := reopened.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("result count before %d, after %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("result %d before %+v, after %+v", i, before[i], after[i])
		}
	}
	if err := reopened.Verify(); err != nil {
		t.Fatal(err)
	}
}
