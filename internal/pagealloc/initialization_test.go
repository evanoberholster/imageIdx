package pagealloc

import "testing"

func TestNewFreeBitmapWordBoundaries(t *testing.T) {
	for _, blocks := range []uint64{2, 3, 63, 64, 65, 127, 128, 129} {
		a, err := New(blocks*uint64(MinBlockSize) + 17)
		if err != nil {
			t.Fatal(err)
		}
		if a.freeCount != blocks-1 {
			t.Fatal(blocks, a.freeCount)
		}
		for block := uint64(0); block < uint64(len(a.free))*64; block++ {
			free := a.free[block>>6]&(uint64(1)<<(block&63)) != 0
			if free != (block > 0 && block < blocks) {
				t.Fatalf("arena %d block %d free %t", blocks, block, free)
			}
		}
		for n := uint64(1); n < blocks; n++ {
			if _, _, err := a.Alloc(MinBlockSize); err != nil {
				t.Fatal(blocks, n, err)
			}
		}
		if _, _, err := a.Alloc(MinBlockSize); err != ErrOutOfSpace {
			t.Fatal(blocks, err)
		}
	}
}
