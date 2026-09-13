package bitmapidx

import (
	"slices"
	"testing"
	"unsafe"

	"github.com/evanoberholster/imageIdx/roar"
)

// Posting representation microbenchmarks: the tagged tiny container
// against one roar.Bitmap per posting, plus mixed-distribution hybrids.

// The tinyPosting type and tinyContains live in posting.go; these benchmarks
// compare it against one roar.Bitmap per posting.

func makeTinyPostings(keys, perKey int) []tinyPosting {
	postings := make([]tinyPosting, keys)
	for index := range postings {
		postings[index].n = uint8(perKey)
		for value := 0; value < perKey; value++ {
			postings[index].ids[value] = uint64(index*perKey + value + 1)
		}
	}
	return postings
}

func makeRoarPostings(keys, perKey int) []roar.Bitmap {
	postings := make([]roar.Bitmap, keys)
	ids := make([]uint64, perKey)
	for index := range postings {
		for value := range ids {
			ids[value] = uint64(index*perKey + value + 1)
		}
		postings[index] = roar.FromUnsorted(ids)
	}
	return postings
}

func makeOverlapRoar(keys, perKey, pool int) []roar.Bitmap {
	postings := make([]roar.Bitmap, keys)
	ids := make([]uint64, perKey)
	for index := range postings {
		for value := range ids {
			ids[value] = uint64((index*perKey+value)%pool) + 1
		}
		postings[index] = roar.FromUnsorted(ids)
	}
	return postings
}

func makeOverlapTiny(keys, perKey, pool int) []tinyPosting {
	postings := make([]tinyPosting, keys)
	for index := range postings {
		postings[index].n = uint8(perKey)
		for value := 0; value < perKey; value++ {
			postings[index].ids[value] = uint64((index*perKey+value)%pool) + 1
		}
	}
	return postings
}

func tinyUnion(dst []uint64, postings []tinyPosting, lo, hi int) []uint64 {
	start := len(dst)
	for index := lo; index < hi; index++ {
		dst = append(dst, postings[index].ids[:postings[index].n]...)
	}
	values := dst[start:]
	slices.Sort(values)
	values = slices.Compact(values)
	return dst[:start+len(values)]
}

func tinyIntersect(dst []uint64, postings []tinyPosting) []uint64 {
	smallest := 0
	for index := range postings {
		if postings[index].n < postings[smallest].n {
			smallest = index
		}
	}
	dst = dst[:0]
	for _, id := range postings[smallest].ids[:postings[smallest].n] {
		present := true
		for index := range postings {
			if index == smallest {
				continue
			}
			if !tinyContains(&postings[index], id) {
				present = false
				break
			}
		}
		if present {
			dst = append(dst, id)
		}
	}
	slices.Sort(dst)
	return dst
}

const (
	tinyKeysOne  = 65_536
	tinyKeysFour = 16_384
	tinySpan     = 1024
)

var (
	tinySinkRoar   []roar.Bitmap
	tinySinkInline []tinyPosting
	tinySinkIDs    []uint64
	tinySinkHits   int
)

func BenchmarkTinyBuildRoar1(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		tinySinkRoar = makeRoarPostings(tinyKeysOne, 1)
	}
}

func BenchmarkTinyBuildInline1(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		tinySinkInline = makeTinyPostings(tinyKeysOne, 1)
	}
}

func BenchmarkTinyBuildRoar4(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		tinySinkRoar = makeRoarPostings(tinyKeysFour, 4)
	}
}

func BenchmarkTinyBuildInline4(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		tinySinkInline = makeTinyPostings(tinyKeysFour, 4)
	}
}

func BenchmarkTinySpaceRoar1(b *testing.B) {
	postings := makeRoarPostings(tinyKeysOne, 1)
	total := 0
	for index := range postings {
		total += postings[index].LenInBytes() + int(unsafe.Sizeof(roar.Bitmap{}))
	}
	b.ReportMetric(float64(total)/float64(len(postings)), "B/posting")
	b.ReportMetric(float64(unsafe.Sizeof(roar.Bitmap{})), "B/header")
	_ = postings
}

func BenchmarkTinySpaceInline1(b *testing.B) {
	b.ReportMetric(float64(unsafe.Sizeof(tinyPosting{})), "B/posting")
}

func BenchmarkTinyOr1024Roar1(b *testing.B) {
	postings := makeRoarPostings(tinyKeysOne, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.OrMany(postings[:tinySpan]...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkTinyOr1024Inline1(b *testing.B) {
	postings := makeTinyPostings(tinyKeysOne, 1)
	dst := make([]uint64, 0, tinySpan)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = tinyUnion(dst[:0], postings, 0, tinySpan)
	}
	tinySinkIDs = dst
}

func BenchmarkTinyRange1024Roar1(b *testing.B) {
	postings := makeRoarPostings(tinyKeysOne, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.OrMany(postings[tinySpan : 2*tinySpan]...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkTinyRange1024Inline1(b *testing.B) {
	postings := makeTinyPostings(tinyKeysOne, 1)
	dst := make([]uint64, 0, tinySpan)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = tinyUnion(dst[:0], postings, tinySpan, 2*tinySpan)
	}
	tinySinkIDs = dst
}

func BenchmarkTinyAnd4Roar4(b *testing.B) {
	postings := makeOverlapRoar(tinyKeysFour, 4, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.AndMany(postings[:4]...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkTinyAnd4Inline4(b *testing.B) {
	postings := makeOverlapTiny(tinyKeysFour, 4, 64)
	dst := make([]uint64, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = tinyIntersect(dst, postings[:4])
	}
	tinySinkIDs = dst
}

func BenchmarkTinyAnd16Roar4(b *testing.B) {
	postings := makeOverlapRoar(tinyKeysFour, 4, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.AndMany(postings[:16]...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkTinyAnd16Inline4(b *testing.B) {
	postings := makeOverlapTiny(tinyKeysFour, 4, 64)
	dst := make([]uint64, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = tinyIntersect(dst, postings[:16])
	}
	tinySinkIDs = dst
}

func BenchmarkTinyContainsRoar1(b *testing.B) {
	postings := makeRoarPostings(tinyKeysOne, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index := n % len(postings)
		if postings[index].Contains(uint64(index + 1)) {
			tinySinkHits++
		}
	}
}

func BenchmarkTinyContainsInline1(b *testing.B) {
	postings := makeTinyPostings(tinyKeysOne, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index := n % len(postings)
		if tinyContains(&postings[index], uint64(index+1)) {
			tinySinkHits++
		}
	}
}

const (
	tinyFilterKeys       = 512
	tinyFilterCandidates = 128
)

func BenchmarkTinyFilterRoar1(b *testing.B) {
	postings := makeRoarPostings(tinyKeysOne, 1)
	candidates := make([]uint64, tinyFilterCandidates)
	for index := range candidates {
		candidates[index] = uint64(index*37 + 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		hits := 0
		for _, id := range candidates {
			for key := 0; key < tinyFilterKeys; key++ {
				if postings[key].Contains(id) {
					hits++
					break
				}
			}
		}
		tinySinkHits = hits
	}
}

func BenchmarkTinyFilterInline1(b *testing.B) {
	postings := makeTinyPostings(tinyKeysOne, 1)
	candidates := make([]uint64, tinyFilterCandidates)
	for index := range candidates {
		candidates[index] = uint64(index*37 + 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		hits := 0
		for _, id := range candidates {
			for key := 0; key < tinyFilterKeys; key++ {
				if tinyContains(&postings[key], id) {
					hits++
					break
				}
			}
		}
		tinySinkHits = hits
	}
}

// Mixed distribution: 80% tiny (1-4 IDs), 15% array containers (5-64),
// 4% larger arrays (65-512), 1% dense bitmap containers (8192 contiguous IDs).
const (
	mixedKeys = 4096
	mixedSpan = 256
	mixedPool = 8192
)

type mixedPosting struct {
	tiny tinyPosting
	bit  roar.Bitmap
}

func (p *mixedPosting) contains(id uint64) bool {
	if p.tiny.n != 0 {
		return tinyContains(&p.tiny, id)
	}
	return p.bit.Contains(id)
}

type mixedFixture struct {
	counts   []int
	roar     []roar.Bitmap
	hybrid   []mixedPosting
	flatOff  []uint32
	flatIDs  []uint64
	flatTiny []bool
}

func mixedCount(index int) int {
	switch remainder := index % 100; {
	case remainder < 80:
		return 1 + index%4
	case remainder < 95:
		return 5 + (index*7)%60
	case remainder < 99:
		return 65 + (index*13)%448
	default:
		return 8192
	}
}

func buildMixedFixture() mixedFixture {
	fixture := mixedFixture{
		counts:   make([]int, mixedKeys),
		roar:     make([]roar.Bitmap, mixedKeys),
		hybrid:   make([]mixedPosting, mixedKeys),
		flatOff:  make([]uint32, mixedKeys+1),
		flatTiny: make([]bool, mixedKeys),
	}
	for index := range fixture.counts {
		count := mixedCount(index)
		ids := make([]uint64, count)
		base := uint64(index+1) << 16
		for value := range ids {
			ids[value] = base + uint64(value)
		}
		fixture.counts[index] = count
		fixture.roar[index] = roar.FromUnsorted(ids)
		if count <= 4 {
			fixture.hybrid[index].tiny.n = uint8(count)
			copy(fixture.hybrid[index].tiny.ids[:], ids)
			fixture.flatTiny[index] = true
			fixture.flatIDs = append(fixture.flatIDs, ids...)
		} else {
			fixture.hybrid[index].bit = fixture.roar[index]
		}
		fixture.flatOff[index+1] = uint32(len(fixture.flatIDs))
	}
	return fixture
}

func BenchmarkMixedSpace(b *testing.B) {
	fixture := buildMixedFixture()
	roarBytes, hybridBytes, flatBytes := 0, 0, 0
	roarHeader := int(unsafe.Sizeof(roar.Bitmap{}))
	tinySize := int(unsafe.Sizeof(tinyPosting{}))
	for index, count := range fixture.counts {
		data := fixture.roar[index].LenInBytes()
		roarBytes += data + roarHeader
		if count <= 4 {
			hybridBytes += tinySize
			flatBytes += count*8 + 4
		} else {
			hybridBytes += data + tinySize
			flatBytes += data + roarHeader
		}
	}
	keys := float64(len(fixture.counts))
	b.ReportMetric(float64(roarBytes)/keys, "roarB/key")
	b.ReportMetric(float64(hybridBytes)/keys, "hybridB/key")
	b.ReportMetric(float64(flatBytes)/keys, "flatB/key")
	for range b.N {
	}
}

func BenchmarkMixedRange256Roar(b *testing.B) {
	fixture := buildMixedFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.OrMany(fixture.roar[:mixedSpan]...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkMixedRange256Hybrid(b *testing.B) {
	fixture := buildMixedFixture()
	mutable := roar.NewMutableBitmap()
	dst := make([]uint64, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		mutable.Reset()
		for index := 0; index < mixedSpan; index++ {
			if fixture.counts[index] <= 4 {
				mutable.AddMany(fixture.hybrid[index].tiny.ids[:fixture.hybrid[index].tiny.n])
			} else {
				mutable.Or(fixture.hybrid[index].bit)
			}
		}
		dst = mutable.AppendTo(dst[:0])
	}
	tinySinkIDs = dst
}

func BenchmarkMixedRange256Flat(b *testing.B) {
	fixture := buildMixedFixture()
	mutable := roar.NewMutableBitmap()
	dst := make([]uint64, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		mutable.Reset()
		for index := 0; index < mixedSpan; index++ {
			if fixture.flatTiny[index] {
				mutable.AddMany(fixture.flatIDs[fixture.flatOff[index]:fixture.flatOff[index+1]])
			} else {
				mutable.Or(fixture.roar[index])
			}
		}
		dst = mutable.AppendTo(dst[:0])
	}
	tinySinkIDs = dst
}

// Overlap fixture: 4, 64, 256 and 8192 IDs drawn from one shared 8192-ID pool,
// so intersections are non-empty and the dense posting lands in a bitmap
// container. IDs stay within one 65536 key block.
func buildOverlapMixed() ([]roar.Bitmap, []mixedPosting, []int) {
	sizes := []int{4, 64, 256, 8192}
	roarPostings := make([]roar.Bitmap, len(sizes))
	hybrid := make([]mixedPosting, len(sizes))
	for index, count := range sizes {
		ids := make([]uint64, count)
		for value := range ids {
			ids[value] = uint64(1)<<20 + uint64(value)
		}
		roarPostings[index] = roar.FromUnsorted(ids)
		if count <= 4 {
			hybrid[index].tiny.n = uint8(count)
			copy(hybrid[index].tiny.ids[:], ids)
		} else {
			hybrid[index].bit = roarPostings[index]
		}
	}
	return roarPostings, hybrid, sizes
}

func BenchmarkMixedAnd4Roar(b *testing.B) {
	roarPostings, _, _ := buildOverlapMixed()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		result := roar.AndMany(roarPostings...)
		tinySinkHits = int(result.Cardinality())
	}
}

func BenchmarkMixedAnd4Hybrid(b *testing.B) {
	_, hybrid, _ := buildOverlapMixed()
	dst := make([]uint64, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = dst[:0]
		for _, id := range hybrid[0].tiny.ids[:hybrid[0].tiny.n] {
			present := true
			for index := 1; index < len(hybrid); index++ {
				if !hybrid[index].contains(id) {
					present = false
					break
				}
			}
			if present {
				dst = append(dst, id)
			}
		}
	}
	tinySinkIDs = dst
}

func BenchmarkMixedContainsRoar(b *testing.B) {
	fixture := buildMixedFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index := n % len(fixture.counts)
		if fixture.roar[index].Contains(uint64(index+1) << 16) {
			tinySinkHits++
		}
	}
}

func BenchmarkMixedContainsHybrid(b *testing.B) {
	fixture := buildMixedFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index := n % len(fixture.counts)
		if fixture.hybrid[index].contains(uint64(index+1) << 16) {
			tinySinkHits++
		}
	}
}

func BenchmarkMixedAppendRoar(b *testing.B) {
	fixture := buildMixedFixture()
	dst := make([]uint64, 0, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		dst = fixture.roar[n%len(fixture.roar)].AppendTo(dst[:0])
	}
	tinySinkIDs = dst
	_ = slices.Contains(dst, 0)
}
