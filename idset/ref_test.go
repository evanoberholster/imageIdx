package idset

import (
	"slices"
	"testing"
)

func TestRefBuilderAndBufferRoundTrip(t *testing.T) {
	ref := New([]uint64{5, 1, 3, 3})
	if !slices.Equal(ref.ToArray(), []uint64{1, 3, 5}) {
		t.Fatalf("New = %v", ref.ToArray())
	}
	builder := Clone(ref)
	builder.AddMany([]uint64{7, 9})
	builder.Remove(3)
	replacement := builder.Freeze()
	if !slices.Equal(replacement.ToArray(), []uint64{1, 5, 7, 9}) {
		t.Fatalf("replacement = %v", replacement.ToArray())
	}
	mapped, err := FromBuffer(replacement.MarshalBinary())
	if err != nil {
		t.Fatalf("FromBuffer = %v", err)
	}
	if !slices.Equal(mapped.ToArray(), replacement.ToArray()) {
		t.Fatalf("mapped = %v", mapped.ToArray())
	}
}

func TestFromBufferRejectsCorrupt(t *testing.T) {
	if _, err := FromBuffer([]byte{0xde, 0xad}); err == nil {
		t.Fatal("FromBuffer accepted corrupt data")
	}
	if _, err := FromBuffer(nil); err == nil {
		t.Fatal("FromBuffer accepted nil")
	}
}

func TestOperands(t *testing.T) {
	left := New([]uint64{1, 2, 3})
	right := New([]uint64{2, 3, 4})
	operands := NewOperands(2)
	operands.Add(left)
	operands.Add(right)
	if got := operands.And().ToArray(); !slices.Equal(got, []uint64{2, 3}) {
		t.Fatalf("And = %v", got)
	}
	if got := operands.Or().ToArray(); !slices.Equal(got, []uint64{1, 2, 3, 4}) {
		t.Fatalf("Or = %v", got)
	}
}

func TestEmptyRef(t *testing.T) {
	empty := Empty()
	if !empty.Valid() || !empty.IsEmpty() || empty.Cardinality() != 0 {
		t.Fatal("Empty is not a valid empty set")
	}
	var zero Ref
	if zero.Valid() {
		t.Fatal("zero Ref reports valid")
	}
	if !zero.IsEmpty() || zero.Cardinality() != 0 || len(zero.ToArray()) != 0 {
		t.Fatal("zero Ref invariants violated")
	}
	if zero.Contains(1) {
		t.Fatal("zero Ref contains an ID")
	}
}
