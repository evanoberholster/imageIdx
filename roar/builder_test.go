package roar

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestFromSortedErrors(t *testing.T) {
	if _, err := FromSorted([]uint64{1 << 48}); err != nil {
		t.Fatalf("FromSorted(1<<48) = %v", err)
	}
	if _, err := FromSorted([]uint64{0}); !is(err, ErrInvalidImageID) {
		t.Fatalf("zero ID error = %v", err)
	}
	if _, err := FromSorted([]uint64{1 << 48, 1 << 48}); !is(err, ErrUnsortedIDs) {
		t.Fatalf("duplicate ID error = %v", err)
	}
	if _, err := FromSorted([]uint64{2 << 48, 1 << 48}); !is(err, ErrUnsortedIDs) {
		t.Fatalf("unsorted ID error = %v", err)
	}
	if _, err := FromSorted([]uint64{1<<48 + 1, 1<<48 + 2, 2<<48 + 1}); err != nil {
		t.Fatalf("valid mixed input rejected: %v", err)
	}
}

func TestSortedBuilder(t *testing.T) {
	ids := []uint64{1<<48 + 1, 1<<48 + 2, 2<<48 + 9, 3<<48 + 65535}
	builder := NewSortedBuilder(0)
	for _, id := range ids {
		if err := builder.Add(id); err != nil {
			t.Fatal(err)
		}
	}
	data, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	view := OpenTrusted(data)
	if view.Cardinality() != uint64(len(ids)) {
		t.Fatalf("Cardinality = %d", view.Cardinality())
	}

	if err := builder.Add(1<<48 + 3); !is(err, ErrSortedBuilderFinished) {
		t.Fatalf("Add after Finish error = %v", err)
	}
	builder.Reset()
	if err := builder.Add(1 << 48); err != nil {
		t.Fatalf("Reset then Add = %v", err)
	}
	again, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if OpenTrusted(again).Cardinality() != 1 {
		t.Fatalf("reused builder Cardinality = %d", OpenTrusted(again).Cardinality())
	}
}

func TestSortedBuilderResetClearsUnfinishedBitmap(t *testing.T) {
	builder := NewSortedBuilder(1)
	for value := uint64(1); value <= ArrayToBitmapThreshold+1; value++ {
		if err := builder.Add(1<<48 | value); err != nil {
			t.Fatal(err)
		}
	}

	builder.Reset()
	want := make([]uint64, ArrayToBitmapThreshold+1)
	for index := range want {
		want[index] = 2<<48 | uint64(10_000+index)
		if err := builder.Add(want[index]); err != nil {
			t.Fatal(err)
		}
	}

	data, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	view, err := Open(data)
	if err != nil {
		t.Fatalf("Open after Reset = %v", err)
	}
	got := view.AppendTo(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("IDs after Reset = %v, want %v", got, want)
	}
}

func TestSortedBuilderAddAfterFinishRequiresReset(t *testing.T) {
	builder := NewSortedBuilder(1)
	if err := builder.Add(1<<48 + 1); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(2<<48 + 1); !is(err, ErrSortedBuilderFinished) {
		t.Fatalf("Add after Finish error = %v", err)
	}

	builder.Reset()
	if err := builder.Add(2<<48 + 1); err != nil {
		t.Fatalf("Add after Reset = %v", err)
	}
	data, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if got := OpenTrusted(data).AppendTo(nil); !slices.Equal(got, []uint64{2<<48 + 1}) {
		t.Fatalf("IDs after Reset = %v", got)
	}
}

func TestArrayToBitmapThreshold(t *testing.T) {
	arrayIDs := make([]uint64, ArrayToBitmapThreshold)
	bitmapIDs := make([]uint64, ArrayToBitmapThreshold+1)
	for index := range bitmapIDs {
		bitmapIDs[index] = 1<<48 | uint64(index+1)
	}
	copy(arrayIDs, bitmapIDs[:ArrayToBitmapThreshold])

	for _, test := range []struct {
		name string
		ids  []uint64
		want ContainerType
	}{
		{"at threshold stays array", arrayIDs, ContainerArray},
		{"above threshold promotes", bitmapIDs, ContainerBitmap},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := FromSorted(test.ids)
			if err != nil {
				t.Fatal(err)
			}
			offset := binary.LittleEndian.Uint64(data[24:32])
			containerType := ContainerType(binary.LittleEndian.Uint16(data[offset+2 : offset+4]))
			if containerType != test.want {
				t.Fatalf("container type = %v, want %v", containerType, test.want)
			}
		})
	}
}

func TestBuilderCompactAndReset(t *testing.T) {
	left, err := FromSorted([]uint64{1<<48 + 1, 1<<48 + 2, 2<<48 + 3})
	if err != nil {
		t.Fatal(err)
	}
	right, err := FromSorted([]uint64{1<<48 + 2, 2<<48 + 3, 2<<48 + 4})
	if err != nil {
		t.Fatal(err)
	}
	builder := NewBuilder(4)
	AndInto(builder, OpenTrusted(left), OpenTrusted(right))
	result := builder.Compact()
	if OpenTrusted(result).Cardinality() != 2 {
		t.Fatalf("AND cardinality = %d", OpenTrusted(result).Cardinality())
	}
	if _, err := Open(result); err != nil {
		t.Fatalf("compacted result invalid: %v", err)
	}
	if builder.KeyCount() != 2 {
		t.Fatalf("KeyCount = %d", builder.KeyCount())
	}
	builder.Reset()
	if builder.KeyCount() != 0 {
		t.Fatalf("KeyCount after Reset = %d", builder.KeyCount())
	}
	AndInto(builder, OpenTrusted(right), OpenTrusted(left))
	if OpenTrusted(builder.Compact()).Cardinality() != 2 {
		t.Fatal("reused builder produced wrong result")
	}
}

func TestBuilderResetReinitializesScratchWhenNeeded(t *testing.T) {
	dense := make([]uint64, ArrayToBitmapThreshold+1)
	for index := range dense {
		dense[index] = 1<<48 | uint64(index+1)
	}
	denseData, err := FromSorted(dense)
	if err != nil {
		t.Fatal(err)
	}

	left := make([]uint64, 1500)
	right := make([]uint64, 1500)
	for index := range left {
		left[index] = 2<<48 | uint64(index+1)
		right[index] = 2<<48 | uint64(index+3001)
	}
	leftData, err := FromSorted(left)
	if err != nil {
		t.Fatal(err)
	}
	rightData, err := FromSorted(right)
	if err != nil {
		t.Fatal(err)
	}

	builder := NewBuilder(1)
	OrInto(builder, OpenTrusted(denseData), OpenTrusted(denseData))
	builder.Reset()
	OrInto(builder, OpenTrusted(leftData), OpenTrusted(rightData))

	want := append(slices.Clone(left), right...)
	got := OpenTrusted(builder.Compact()).AppendTo(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("IDs after Reset = %v, want %v", got, want)
	}
}

func is(err error, target error) bool {
	return err == target
}
