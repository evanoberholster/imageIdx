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
	mapped := FromBuffer(replacement.MarshalBinary())
	if !slices.Equal(mapped.ToArray(), replacement.ToArray()) {
		t.Fatalf("mapped = %v", mapped.ToArray())
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
