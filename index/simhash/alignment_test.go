package simhash

import (
	"sync/atomic"
	"testing"
	"unsafe"
)

// TestAtomicColumnAlignment checks that every atomic uint64 column access is
// 8-byte aligned, including the boundary slots of an 8-byte quality group and
// the last slot of a hash page.
func TestAtomicColumnAlignment(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(10, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	v := st.view.Load()
	sh, pi := v.shardForHashPage(0)
	for _, slot := range []int{0, 1, 7, 8, 9, 15, 65471, 65528, 65535} {
		if uintptr(unsafe.Pointer(sh.phash(pi, slot)))%8 != 0 {
			t.Fatalf("pHash slot %d not 8-aligned", slot)
		}
		if uintptr(unsafe.Pointer(sh.pdqWord(pi, slot, 0)))%8 != 0 {
			t.Fatalf("PDQ slot %d not 8-aligned", slot)
		}
		if uintptr(unsafe.Pointer(sh.qualityWord(pi, slot)))%8 != 0 {
			t.Fatalf("quality slot %d not 8-aligned", slot)
		}
	}

	// Read-modify-write of each quality byte in the first and last 8-byte group
	// must not disturb its neighbours.
	for _, base := range []int{0, 65528} {
		for s := base; s < base+8; s++ {
			shift := qualityShift(s)
			qw := sh.qualityWord(pi, s)
			atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(s&0xff)<<shift))
		}
		for s := base; s < base+8; s++ {
			if got := uint8(atomic.LoadUint64(sh.qualityWord(pi, s)) >> qualityShift(s)); got != uint8(s&0xff) {
				t.Fatalf("quality slot %d = %#x, want %#x", s, got, uint8(s&0xff))
			}
		}
	}
}

func TestColumnOffsetsAligned(t *testing.T) {
	for name, off := range map[string]uint64{
		"pHash":   pHashColumnOffset,
		"PDQ":     pdqColumnOffset,
		"quality": qualityColumnOffset,
		"seq":     seqOffset,
	} {
		if off%64 != 0 {
			t.Fatalf("%s column offset %d not 64-aligned", name, off)
		}
	}
	if headerBytes%64 != 0 {
		t.Fatal("header not 64-aligned")
	}
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		if s := newGeometry(seq).hashPageStride; s%64 != 0 {
			t.Fatalf("seq %d hash page stride %d not 64-aligned", seq, s)
		}
	}
}
