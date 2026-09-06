package roar

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestMutableBuilderAddRemove(t *testing.T) {
	base, err := FromSortedList([]uint64{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	builder := NewMutableBuilderFrom(base)
	builder.AddMany([]uint64{6, 7, 8})
	builder.Remove(3)
	builder.Remove(9)

	got := builder.Freeze().AppendTo(nil)
	want := []uint64{1, 2, 4, 5, 6, 7, 8}
	if !slices.Equal(got, want) {
		t.Fatalf("Freeze = %v, want %v", got, want)
	}
}

func TestMutableBuilderEmptyStart(t *testing.T) {
	builder := NewMutableBuilder()
	builder.AddMany([]uint64{5, 1, 3})
	builder.Remove(1)
	got := builder.Freeze().AppendTo(nil)
	if !slices.Equal(got, []uint64{3, 5}) {
		t.Fatalf("Freeze = %v, want [3 5]", got)
	}
}

func TestMutableBuilderRemovalWinsOverAdd(t *testing.T) {
	builder := NewMutableBuilder()
	builder.AddMany([]uint64{7})
	builder.Remove(7)
	got := builder.Freeze().AppendTo(nil)
	if len(got) != 0 {
		t.Fatalf("Freeze = %v, want empty", got)
	}
}

func TestMutableBuilderSetOps(t *testing.T) {
	base, _ := FromSortedList([]uint64{1, 2, 3})
	other, _ := FromSortedList([]uint64{2, 3, 4})

	or := NewMutableBuilderFrom(base)
	or.Or(other)
	if got, want := or.Freeze().AppendTo(nil), []uint64{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("Or = %v, want %v", got, want)
	}

	and := NewMutableBuilderFrom(base)
	and.And(other)
	if got, want := and.Freeze().AppendTo(nil), []uint64{2, 3}; !slices.Equal(got, want) {
		t.Fatalf("And = %v, want %v", got, want)
	}

	andNot := NewMutableBuilderFrom(base)
	andNot.AndNot(other)
	if got, want := andNot.Freeze().AppendTo(nil), []uint64{1}; !slices.Equal(got, want) {
		t.Fatalf("AndNot = %v, want %v", got, want)
	}
}

func TestMutableBuilderFlushBeforeSetOp(t *testing.T) {
	base, _ := FromSortedList([]uint64{1, 2, 3})
	constraint, _ := FromSortedList([]uint64{1, 3, 5})

	builder := NewMutableBuilderFrom(base)
	builder.AddMany([]uint64{4})
	builder.And(constraint)
	if got, want := builder.Freeze().AppendTo(nil), []uint64{1, 3}; !slices.Equal(got, want) {
		t.Fatalf("And after AddMany = %v, want %v", got, want)
	}
}

func TestMutableBuilderReset(t *testing.T) {
	builder := NewMutableBuilder()
	builder.AddMany([]uint64{1, 2, 3})
	if got := builder.Freeze().AppendTo(nil); len(got) != 3 {
		t.Fatalf("Freeze = %v, want 3 IDs", got)
	}
	builder.Reset()
	builder.AddMany([]uint64{9})
	if got, want := builder.Freeze().AppendTo(nil), []uint64{9}; !slices.Equal(got, want) {
		t.Fatalf("after Reset = %v, want %v", got, want)
	}
}

func TestMutableBuilderIgnoresZero(t *testing.T) {
	builder := NewMutableBuilder()
	builder.AddMany([]uint64{0, 1, 2})
	builder.Remove(0)
	if got, want := builder.Freeze().AppendTo(nil), []uint64{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("Freeze = %v, want %v", got, want)
	}
}

func TestMutableBuilderMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for round := 0; round < 50; round++ {
		pool := keyPool(rng, 16)
		base := randomSetOver(rng, pool)
		adds := randomSetOver(rng, pool)
		removes := randomSetOver(rng, pool)

		baseBm, _ := FromSortedList(base)
		addBm, _ := FromSortedList(adds)
		removeBm, _ := FromSortedList(removes)

		builder := NewMutableBuilderFrom(baseBm)
		builder.AddMany(adds)
		for _, id := range removes {
			builder.Remove(id)
		}
		got := builder.Freeze().AppendTo(nil)

		want := AndNot(Or(baseBm, addBm), removeBm).AppendTo(nil)
		if !slices.Equal(got, want) {
			t.Fatalf("round %d: mutable builder mismatch", round)
		}
	}
}

func TestMutableBuilderInlineBuffersAvoidAllocation(t *testing.T) {
	ids := make([]uint64, inlineDeltaCapacity)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	builder := NewMutableBuilder()
	allocs := testing.AllocsPerRun(100, func() {
		builder.Reset()
		builder.AddMany(ids)
		for _, id := range ids {
			builder.Remove(id)
		}
	})
	if allocs != 0 {
		t.Fatalf("in-buffer mutations allocated %v times, want 0", allocs)
	}
}
