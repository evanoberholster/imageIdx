package pagealloc

import (
	"encoding/binary"
	"errors"
	"testing"
)

const testArenaBlocks = 64

func newTestAllocator(t *testing.T) *Allocator {
	t.Helper()
	allocator, err := New(testArenaBlocks * uint64(MinBlockSize))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := allocator.Validate(); err != nil {
		t.Fatalf("fresh allocator invalid: %v", err)
	}
	return allocator
}

func TestNewRejectsTinyArena(t *testing.T) {
	for _, size := range []uint64{0, uint64(MinBlockSize), uint64(MinBlockSize) + 1} {
		if _, err := New(size); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("New(%d) = %v, want ErrTooLarge", size, err)
		}
	}
}

func TestNewRoundsDownToWholeBlocks(t *testing.T) {
	allocator, err := New(2*uint64(MinBlockSize) + 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := allocator.TotalBytes(), 2*uint64(MinBlockSize); got != want {
		t.Fatalf("TotalBytes = %d, want %d", got, want)
	}
	if got, want := allocator.FreeBytes(), uint64(MinBlockSize); got != want {
		t.Fatalf("FreeBytes = %d, want %d", got, want)
	}
}

func TestAllocIsExactFit(t *testing.T) {
	allocator := newTestAllocator(t)
	cases := []struct {
		size uint32
		want uint32
	}{
		{0, MinBlockSize},
		{1, MinBlockSize},
		{MinBlockSize, MinBlockSize},
		{MinBlockSize + 1, 2 * MinBlockSize},
		{12 * MinBlockSize, 12 * MinBlockSize},
		{12*MinBlockSize + 1, 13 * MinBlockSize},
	}
	for _, tc := range cases {
		ref, capacity, err := allocator.Alloc(tc.size)
		if err != nil {
			t.Fatalf("Alloc(%d): %v", tc.size, err)
		}
		if ref == 0 {
			t.Fatalf("Alloc(%d) returned the null ref", tc.size)
		}
		if capacity != tc.want {
			t.Fatalf("Alloc(%d) capacity = %d, want %d", tc.size, capacity, tc.want)
		}
		if capacity%MinBlockSize != 0 {
			t.Fatalf("Alloc(%d) capacity %d is not a block multiple", tc.size, capacity)
		}
		if err := allocator.Validate(); err != nil {
			t.Fatalf("Validate after Alloc(%d): %v", tc.size, err)
		}
	}
}

func TestAllocTooLarge(t *testing.T) {
	allocator := newTestAllocator(t)
	if _, _, err := allocator.Alloc(uint32(allocator.TotalBytes()) + MinBlockSize); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Alloc oversized = %v, want ErrTooLarge", err)
	}
}

func TestExhaustAndCoalesce(t *testing.T) {
	allocator := newTestAllocator(t)
	initial := allocator.FreeBytes()
	wantBlocks := allocator.TotalBlocks() - 1
	type extent struct {
		ref Ref
		cap uint32
	}
	extents := make([]extent, 0, wantBlocks)
	for uint64(len(extents)) < wantBlocks {
		ref, capacity, err := allocator.Alloc(MinBlockSize)
		if err != nil {
			t.Fatalf("Alloc %d: %v", len(extents), err)
		}
		extents = append(extents, extent{ref, capacity})
		if err := allocator.Validate(); err != nil {
			t.Fatalf("Validate during exhaustion: %v", err)
		}
	}
	if free := allocator.FreeBytes(); free != 0 {
		t.Fatalf("FreeBytes after exhaustion = %d, want 0", free)
	}
	if _, _, err := allocator.Alloc(MinBlockSize); !errors.Is(err, ErrOutOfSpace) {
		t.Fatalf("Alloc past exhaustion = %v, want ErrOutOfSpace", err)
	}
	for _, extent := range extents {
		if err := allocator.Retire(extent.ref, extent.cap); err != nil {
			t.Fatalf("Retire(%d): %v", extent.ref, err)
		}
	}
	if err := allocator.Reclaim(); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if free := allocator.FreeBytes(); free != initial {
		t.Fatalf("FreeBytes after reclaim = %d, want %d", free, initial)
	}
	if err := allocator.Validate(); err != nil {
		t.Fatalf("Validate after reclaim: %v", err)
	}
}

func TestFreeAndReuseExactRun(t *testing.T) {
	allocator := newTestAllocator(t)
	initial := allocator.FreeBytes()
	ref, capacity, err := allocator.Alloc(12 * MinBlockSize)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := allocator.Free(ref, capacity); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if free := allocator.FreeBytes(); free != initial {
		t.Fatalf("FreeBytes = %d, want %d", free, initial)
	}
	if err := allocator.Free(ref, capacity); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("double Free = %v, want ErrInvalidRef", err)
	}
	if err := allocator.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRetireReclaim(t *testing.T) {
	allocator := newTestAllocator(t)
	initial := allocator.FreeBytes()
	ref, capacity, err := allocator.Alloc(MinBlockSize)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := allocator.Retire(ref, capacity); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if allocator.Retired() != 1 {
		t.Fatalf("Retired = %d, want 1", allocator.Retired())
	}
	if allocator.RetiredBytes() != uint64(capacity) {
		t.Fatalf("RetiredBytes = %d, want %d", allocator.RetiredBytes(), capacity)
	}
	if err := allocator.Reclaim(); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if allocator.Retired() != 0 {
		t.Fatalf("Retired = %d, want 0", allocator.Retired())
	}
	if free := allocator.FreeBytes(); free != initial {
		t.Fatalf("FreeBytes = %d, want %d", free, initial)
	}
}

func TestRetireRejectsBadReference(t *testing.T) {
	allocator := newTestAllocator(t)
	if err := allocator.Retire(0, MinBlockSize); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Retire(null) = %v, want ErrInvalidRef", err)
	}
	if err := allocator.Retire(Ref(allocator.TotalBlocks()), MinBlockSize); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Retire(out of range) = %v, want ErrInvalidRef", err)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	allocator := newTestAllocator(t)
	for i := 0; i < 9; i++ {
		size := MinBlockSize * uint32(1+i%3)
		if _, _, err := allocator.Alloc(size); err != nil {
			t.Fatalf("Alloc: %v", err)
		}
	}
	encoded := allocator.Encode()
	restored, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatalf("restored allocator invalid: %v", err)
	}
	if restored.TotalBytes() != allocator.TotalBytes() {
		t.Fatalf("TotalBytes = %d, want %d", restored.TotalBytes(), allocator.TotalBytes())
	}
	if restored.FreeBytes() != allocator.FreeBytes() {
		t.Fatalf("FreeBytes = %d, want %d", restored.FreeBytes(), allocator.FreeBytes())
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	allocator := newTestAllocator(t)
	encoded := allocator.Encode()

	truncated := append([]byte(nil), encoded[:len(encoded)-3]...)
	if _, err := Decode(truncated); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(truncated) = %v, want ErrCorrupt", err)
	}
	badMagic := append([]byte(nil), encoded...)
	binary.LittleEndian.PutUint32(badMagic[0:4], 0xdeadbeef)
	if _, err := Decode(badMagic); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(bad magic) = %v, want ErrCorrupt", err)
	}
	badCRC := append([]byte(nil), encoded...)
	badCRC[len(badCRC)/2] ^= 0xff
	if _, err := Decode(badCRC); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(bad crc) = %v, want ErrCorrupt", err)
	}
	badVersion := append([]byte(nil), encoded...)
	binary.LittleEndian.PutUint16(badVersion[4:6], metadataVersion+1)
	if _, err := Decode(badVersion); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Decode(bad version) = %v, want ErrCorrupt", err)
	}
}

func TestReserveMarksExtentUsed(t *testing.T) {
	allocator := newTestAllocator(t)
	initial := allocator.FreeBytes()
	const blocks = 3
	if err := allocator.Reserve(1, blocks*MinBlockSize); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got, want := allocator.FreeBytes(), initial-blocks*uint64(MinBlockSize); got != want {
		t.Fatalf("FreeBytes = %d, want %d", got, want)
	}
	if err := allocator.Reserve(1, MinBlockSize); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("double Reserve = %v, want ErrInvalidRef", err)
	}
	if err := allocator.Reserve(0, MinBlockSize); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Reserve(reserved block) = %v, want ErrInvalidRef", err)
	}
	if err := allocator.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ref, _, err := allocator.Alloc(MinBlockSize)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if ref < 4 {
		t.Fatalf("Alloc returned reserved block %d", ref)
	}
}

func FuzzAllocator(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{1, 0, 2, 1, 0, 2, 1, 0, 2, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		allocator, err := New(64 * uint64(MinBlockSize))
		if err != nil {
			t.Skip()
		}
		initial := allocator.FreeBytes()
		type live struct {
			ref  Ref
			size uint32
		}
		var refs []live
		for _, op := range data {
			switch op % 4 {
			case 0:
				size := MinBlockSize * uint32(1+op%8)
				ref, capacity, err := allocator.Alloc(size)
				if err == nil {
					refs = append(refs, live{ref, capacity})
				} else if !errors.Is(err, ErrOutOfSpace) && !errors.Is(err, ErrTooLarge) {
					t.Fatalf("Alloc: %v", err)
				}
			case 1:
				if len(refs) == 0 {
					break
				}
				index := int(op) % len(refs)
				entry := refs[index]
				if err := allocator.Retire(entry.ref, entry.size); err != nil {
					t.Fatalf("Retire: %v", err)
				}
				refs[index] = refs[len(refs)-1]
				refs = refs[:len(refs)-1]
			case 2:
				if err := allocator.Reclaim(); err != nil {
					t.Fatalf("Reclaim: %v", err)
				}
			case 3:
				encoded := allocator.Encode()
				restored, err := Decode(encoded)
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if restored.FreeBytes() != allocator.FreeBytes() {
					t.Fatalf("round trip FreeBytes = %d, want %d", restored.FreeBytes(), allocator.FreeBytes())
				}
			}
			if err := allocator.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		}
		for _, entry := range refs {
			if err := allocator.Retire(entry.ref, entry.size); err != nil {
				t.Fatalf("Retire: %v", err)
			}
		}
		if err := allocator.Reclaim(); err != nil {
			t.Fatalf("Reclaim: %v", err)
		}
		if allocator.FreeBytes() != initial {
			t.Fatalf("FreeBytes after drain = %d, want %d", allocator.FreeBytes(), initial)
		}
	})
}
