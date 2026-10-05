package simhash

import "testing"

func TestAddressMapping(t *testing.T) {
	cases := []struct {
		id   uint64
		page uint64
		slot int
	}{
		{1, 0, 1},
		{0xffff, 0, 0xffff},
		{0x10000, 1, 0},
		{0x10001, 1, 1},
		{0x1ffff, 1, 0xffff},
		{0x20000, 2, 0},
		{0x1234567, 0x123, 0x4567},
	}
	for _, c := range cases {
		if got := hashPageOf(c.id); got != c.page {
			t.Fatalf("hashPageOf(%#x) = %#x, want %#x", c.id, got, c.page)
		}
		if got := slotOf(c.id); got != c.slot {
			t.Fatalf("slotOf(%#x) = %#x, want %#x", c.id, got, c.slot)
		}
		if got := imageIDFromHashPage(c.page, c.slot); got != c.id {
			t.Fatalf("imageIDFromHashPage(%#x,%#x) = %#x, want %#x", c.page, c.slot, got, c.id)
		}
	}
}

func TestDistances(t *testing.T) {
	if got := DistancePHash(0, 0); got != 0 {
		t.Fatalf("self pHash distance = %d", got)
	}
	if got := DistancePHash(0, 0xf); got != 4 {
		t.Fatalf("pHash distance = %d, want 4", got)
	}
	var a, b PDQHash
	b[0] = 1
	b[31] = 0x80
	if got := DistancePDQ(&a, &a); got != 0 {
		t.Fatalf("self PDQ distance = %d", got)
	}
	if got := DistancePDQ(&a, &b); got != 2 {
		t.Fatalf("PDQ distance = %d, want 2", got)
	}
}
