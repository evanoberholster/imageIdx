package vectoridx

import (
	"math/rand"
	"testing"
)

func TestSpaceIDPacking(t *testing.T) {
	for _, c := range []struct {
		idType IndexType
		space  uint8
		valid  bool
	}{
		{IndexTypeImage, 0, true},
		{IndexTypeImage, 255, true},
		{IndexTypeFace, 7, true},
		{IndexTypeCustom, 200, true},
		{IndexType(0), 1, false},
		{IndexType(99), 1, false},
	} {
		packed := NewSpaceID(c.idType, c.space)
		if packed.Valid() != c.valid {
			t.Fatalf("NewSpaceID(%d, %d).Valid() = %v, want %v", c.idType, c.space, packed.Valid(), c.valid)
		}
		if !c.valid {
			continue
		}
		if packed.Type() != c.idType || packed.Space() != c.space {
			t.Fatalf("NewSpaceID(%d, %d) = idType %d space %d", c.idType, c.space, packed.Type(), packed.Space())
		}
	}
	if SpaceID(0).Valid() {
		t.Fatal("zero SpaceID reported valid")
	}
}

func TestDimensionsValidation(t *testing.T) {
	for _, d := range []Dimensions{Dim512, Dim768, Dim1024} {
		if !d.Valid() {
			t.Fatalf("%d reported invalid", d)
		}
	}
	for _, d := range []Dimensions{0, 100, 513, 2048} {
		if d.Valid() {
			t.Fatalf("%d reported valid", d)
		}
	}
}

// TestSpaceIDAndResultAcrossReopen pins the packed identity in the header and
// the NodeID -> ImageID widening in results.
func TestSpaceIDAndResultAcrossReopen(t *testing.T) {
	path := tempPath(t)
	packed := NewSpaceID(IndexTypeFace, 5)
	idx, err := Create(path, Config{SpaceID: packed, Dimensions: Dim512, ElementType: ElementFloat32}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	vec := randFloatVec(rand.New(rand.NewSource(101)), Dim512)
	const id = NodeID(4000000000) // above 2^31, still within uint32
	if err := idx.AddFloat32(id, vec); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Config().SpaceID
	if got != packed || got.Type() != IndexTypeFace || got.Space() != 5 {
		t.Fatalf("reopened SpaceID = %d (type %d space %d), want %d", got, got.Type(), got.Space(), packed)
	}
	results, err := reopened.SearchFloat32(vec, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != ImageID(id) || results[0].Score != 0 {
		t.Fatalf("result = %+v, want ImageID %d Score 0", results, id)
	}
}
