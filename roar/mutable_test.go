package roar

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestMutableMatchesImmutable applies a deterministic mixed sequence of
// accumulations and whole-set folds to a MutableBitmap and mirrors every
// step with immutable Bitmap operations, comparing materializations.
func TestMutableMatchesImmutable(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	m := NewMutableBitmap()
	shadow := Bitmap{}

	// Seed bitmaps with overlapping dense and sparse ID populations.
	seeds := make([]Bitmap, 4)
	for i := range seeds {
		ids := make([]uint64, 0, 300)
		for n := 0; n < 300; n++ {
			ids = append(ids, uint64(1+rng.IntN(5_000)+i*1_000))
		}
		seeds[i] = FromUnsorted(ids)
	}

	applyShadow := func(op int, ids []uint64, other Bitmap) {
		switch op {
		case 0, 1:
			// AddMany: fold staged IDs through a canonical add bitmap.
			staged := FromUnsorted(ids)
			shadow = Or(shadow, staged)
		case 2:
			shadow = Or(shadow, other)
		case 3:
			shadow = And(shadow, other)
		case 4:
			shadow = AndNot(shadow, other)
		case 5:
			for _, id := range ids {
				if id == InvalidImageID {
					continue
				}
				single := FromUnsorted([]uint64{id})
				shadow = AndNot(shadow, single)
			}
		}
	}

	check := func(step int) {
		t.Helper()
		if got, want := m.Cardinality(), shadow.Cardinality(); got != want {
			t.Fatalf("step %d: Cardinality = %d, want %d", step, got, want)
		}
		if got, want := m.AppendTo(nil), shadow.AppendTo(nil); !slices.Equal(got, want) {
			t.Fatalf("step %d: AppendTo mismatch (got %d IDs, want %d)", step, len(got), len(want))
		}
	}

	for step := 0; step < 200; step++ {
		ids := make([]uint64, 1+rng.IntN(24))
		for i := range ids {
			// Include duplicates and the reserved zero to exercise
			// canonicalization.
			ids[i] = uint64(rng.IntN(6_000))
		}
		other := seeds[rng.IntN(len(seeds))]
		op := rng.IntN(6)
		switch op {
		case 0, 1:
			m.AddMany(ids)
		case 2:
			m.Or(other)
		case 3:
			m.And(other)
		case 4:
			m.AndNot(other)
		case 5:
			for _, id := range ids {
				m.Remove(id)
			}
		}
		applyShadow(op, ids, other)
		// Reset midway and rebuild from the shadow to cover reuse.
		if step == 100 {
			m.Reset()
			m.Or(shadow)
		}
		check(step)
	}

	frozen := m.Freeze()
	if got, want := frozen.AppendTo(nil), shadow.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatal("Freeze diverges from shadow")
	}
	// Accumulating after a Freeze must include the frozen state: the
	// materialize fast path may only run with no retained bitmap.
	m.AddMany([]uint64{7_001, 7_002})
	shadow = Or(shadow, FromUnsorted([]uint64{7_001, 7_002}))
	if got, want := m.AppendTo(nil), shadow.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("AppendTo after Freeze+AddMany = %v, want %v", got, want)
	}
	if got, want := m.Cardinality(), shadow.Cardinality(); got != want {
		t.Fatalf("Cardinality after Freeze+AddMany = %d, want %d", got, want)
	}
	// The instance stays usable after Freeze and after Reset.
	m.Reset()
	if got := m.Cardinality(); got != 0 {
		t.Fatalf("Cardinality after Reset = %d, want 0", got)
	}
	if got := m.AppendTo(nil); len(got) != 0 {
		t.Fatalf("AppendTo after Reset = %v, want empty", got)
	}
}

// TestMutableZeroValueUsable ensures a MutableBitmap needs no constructor:
// declaration plus Reset is sufficient.
func TestMutableZeroValueUsable(t *testing.T) {
	var m MutableBitmap
	m.Reset()
	m.AddMany([]uint64{3, 1, 2, 2, 0})
	other := FromUnsorted([]uint64{2, 4})
	m.Or(other)
	if got, want := m.AppendTo(nil), []uint64{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("AppendTo = %v, want %v", got, want)
	}
	if got := m.Cardinality(); got != 4 {
		t.Fatalf("Cardinality = %d, want 4", got)
	}
}

// TestAddManyCopiesInput proves AddMany copies the staged IDs rather than
// retaining or aliasing the caller's slice. This is the invariant that lets a
// reused staging buffer (for example geoidx's pooled collector) be overwritten
// immediately after the call without corrupting the bitmap. If AddMany ever
// starts retaining the slice, this test fails and the pool becomes unsafe.
func TestAddManyCopiesInput(t *testing.T) {
	want := []uint64{1, 3, 5, 7}
	newSource := func() []uint64 { return []uint64{7, 3, 5, 1, 3, 0} }

	m := NewMutableBitmap()
	source := newSource()
	m.AddMany(source)
	for i := range source {
		source[i] = 999
	}
	if got := m.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("MutableBitmap.AppendTo after source overwrite = %v, want %v", got, want)
	}

	builder := NewDeltaBuilder()
	source = newSource()
	builder.AddMany(source)
	for i := range source {
		source[i] = 999
	}
	if got := builder.Freeze().AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("DeltaBuilder.Freeze after source overwrite = %v, want %v", got, want)
	}
}
