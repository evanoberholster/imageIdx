package radixhash

import (
	"fmt"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// layoutLeaf builds one depth-1 leaf holding count distinct slots at pagealloc
// reference 1 and returns a query hash that matches the middle slot, so each
// search confirms a hit.
func layoutLeaf(count int) (arena []byte, ref pagealloc.Ref, header leafHeader, query Hash) {
	slots := make([]slot, count)
	mid := count / 2
	for i := 0; i < count; i++ {
		h := layoutHash(uint64(i) + 1)
		copy(slots[i].tail[:], h[1:])
		slots[i].id = uint64(i + 1)
	}
	sortSlots(slots, tailLenFor(1))
	extentBytes := uint32(((sizeForCapacity(1, id32Bytes, count) + 511) / 512) * 512)
	arena = make([]byte, int(pagealloc.MinBlockSize)+int(extentBytes))
	if err := encodeLeaf(arena[int(pagealloc.MinBlockSize):], 1, slots); err != nil {
		panic(err)
	}
	ref = pagealloc.Ref(1)
	header, err := parseLeafHeader(arena, ref)
	if err != nil {
		panic(err)
	}
	return arena, ref, header, layoutHash(uint64(mid) + 1)
}

func benchLeafSearch(b *testing.B, count int) {
	arena, ref, header, query := layoutLeaf(count)
	const depth = 1
	visit := func(ImageID) bool { return true }
	b.Run("linear", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeafLinear(arena, ref, query, depth, header, visit)
		}
	})
	b.Run("binary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeafBinary(arena, ref, query, depth, header, visit)
		}
	})
	b.Run("dispatch", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeaf(arena, ref, query, depth, visit)
		}
	})
}

// BenchmarkLeafSearchCrossover measures the SIMD linear scan against binary
// search across realistic small-leaf occupancies. Linear is only valid up to
// tailkernel's 64-slot mask, so the crossover lives in this range.
func BenchmarkLeafSearchCrossover(b *testing.B) {
	for _, count := range []int{4, 8, 12, 16, 24, 32, 48, 64} {
		b.Run(fmt.Sprintf("count=%d", count), func(b *testing.B) { benchLeafSearch(b, count) })
	}
}
