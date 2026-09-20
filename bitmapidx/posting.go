package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/roar"
)

// tinyCapacity is the maximum number of IDs stored inline in a tiny
// posting. Four keeps the struct at 40 bytes and covers the long tail of
// rare values without a bitmap allocation.
const tinyCapacity = 4

// tinyPosting stores up to tinyCapacity sorted IDs inline, avoiding a
// bitmap allocation for rare values. IDs are sorted ascending for linear
// probes and direct bitmap builds.
type tinyPosting struct {
	n   uint8
	ids [tinyCapacity]uint64
}

// tinyContains linearly probes the inline IDs. With at most four entries
// a scan beats any index.
func tinyContains(p *tinyPosting, id uint64) bool {
	for index := 0; index < int(p.n); index++ {
		if p.ids[index] == id {
			return true
		}
	}
	return false
}

// posting is the tagged posting representation: an inline container for
// up to tinyCapacity IDs, otherwise a roaring bitmap. The zero value is
// an empty posting (not tiny, empty bitmap). Bitmap postings always hold
// more than tinyCapacity IDs; see makePosting.
type posting struct {
	tiny tinyPosting
	bit  roar.Bitmap
}

// isTiny reports whether the posting uses inline storage. Empty
// postings report false and fall through to the empty bitmap path.
func (p *posting) isTiny() bool { return p.tiny.n != 0 }

// isEmpty reports whether the posting holds no IDs.
func (p *posting) isEmpty() bool {
	if p.isTiny() {
		return false
	}
	return p.bit.IsEmpty()
}

// cardinality returns the number of IDs without materializing.
func (p *posting) cardinality() uint64 {
	if p.isTiny() {
		return uint64(p.tiny.n)
	}
	return p.bit.Cardinality()
}

// contains reports whether id is present.
func (p *posting) contains(id uint64) bool {
	if p.isTiny() {
		return tinyContains(&p.tiny, id)
	}
	return p.bit.Contains(id)
}

// appendTo appends all IDs in ascending order to dst.
func (p *posting) appendTo(dst []ImageID) []ImageID {
	if p.isTiny() {
		return append(dst, p.tiny.ids[:p.tiny.n]...)
	}
	return p.bit.AppendTo(dst)
}

// addTo folds the posting into a caller-owned mutable bitmap. It is the
// zero-alloc accumulation path for geo and full scans.
func (p *posting) addTo(mutable *roar.MutableBitmap) {
	if p.isTiny() {
		mutable.AddMany(p.tiny.ids[:p.tiny.n])
		return
	}
	mutable.Or(p.bit)
}

// bitmap materializes the posting as a roaring bitmap. Tiny postings
// build a small fresh bitmap; large postings return the shared immutable
// bitmap without copying.
func (p *posting) bitmap() roar.Bitmap {
	if p.isTiny() {
		return roar.FromUnsorted(p.tiny.ids[:p.tiny.n])
	}
	return p.bit
}

// makePosting wraps a bitmap, demoting postings of at most tinyCapacity
// IDs to the inline form. Empty bitmaps become the zero posting. The
// bitmap is not retained in tiny form; its IDs are copied inline.
func makePosting(bit roar.Bitmap) posting {
	cardinality := bit.Cardinality()
	if cardinality == 0 {
		return posting{}
	}
	if cardinality > tinyCapacity {
		return posting{bit: bit}
	}
	var p posting
	p.tiny.n = uint8(cardinality)
	bit.AppendTo(p.tiny.ids[:0])
	return p
}

// intersectPostings intersects effective postings. When the smallest
// operand is tiny its IDs probe the others with no bitmap builds;
// otherwise operands materialize and intersect with the roaring N-way
// kernel. Uses pooled bitmap scratch and never retains it.
func intersectPostings(postings []*posting) roar.Bitmap {
	if len(postings) == 0 {
		return roar.Bitmap{}
	}
	smallest := 0
	for index := range postings {
		if postings[index].cardinality() < postings[smallest].cardinality() {
			smallest = index
		}
	}
	if probe := postings[smallest]; probe.isTiny() {
		var buf [tinyCapacity]uint64
		ids := buf[:0]
		for _, id := range probe.tiny.ids[:probe.tiny.n] {
			present := true
			for index := range postings {
				if index == smallest {
					continue
				}
				if !postings[index].contains(id) {
					present = false
					break
				}
			}
			if present {
				ids = append(ids, id)
			}
		}
		return roar.FromUnsorted(ids)
	}
	bitmaps := acquireBitmaps(len(postings))
	for index := range postings {
		*bitmaps = append(*bitmaps, postings[index].bitmap())
	}
	out := roar.AndMany(*bitmaps...)
	releaseBitmaps(bitmaps)
	return out
}

// intersectPostingsCardinality counts the intersection without
// materializing it. Tiny-smallest probes count directly; otherwise the
// N-way cardinality kernel runs over pooled scratch.
func intersectPostingsCardinality(postings []*posting) uint64 {
	if len(postings) == 0 {
		return 0
	}
	smallest := 0
	for index := range postings {
		if postings[index].cardinality() < postings[smallest].cardinality() {
			smallest = index
		}
	}
	if probe := postings[smallest]; probe.isTiny() {
		var count uint64
		for _, id := range probe.tiny.ids[:probe.tiny.n] {
			present := true
			for index := range postings {
				if index == smallest {
					continue
				}
				if !postings[index].contains(id) {
					present = false
					break
				}
			}
			if present {
				count++
			}
		}
		return count
	}
	bitmaps := acquireBitmaps(len(postings))
	for index := range postings {
		*bitmaps = append(*bitmaps, postings[index].bitmap())
	}
	out := roar.AndManyCardinality(*bitmaps...)
	releaseBitmaps(bitmaps)
	return out
}

// unionPostings unions effective postings. All-tiny unions collect IDs
// into pooled scratch and build once; other unions run the immutable
// roaring N-way kernel over pooled scratch. Mutable bitmaps are reserved
// for the caller-owned geo accumulation path (CollectGeoRanges).
func unionPostings(postings []*posting) roar.Bitmap {
	switch len(postings) {
	case 0:
		return roar.Bitmap{}
	case 1:
		return postings[0].bitmap()
	}
	allTiny, total := true, 0
	for index := range postings {
		if postings[index].isTiny() {
			total += int(postings[index].tiny.n)
		} else {
			allTiny = false
		}
	}
	if allTiny {
		ids := acquireIDs(total)
		for index := range postings {
			*ids = append(*ids, postings[index].tiny.ids[:postings[index].tiny.n]...)
		}
		out := roar.FromUnsorted(*ids)
		releaseIDs(ids)
		return out
	}
	bitmaps := acquireBitmaps(len(postings))
	for index := range postings {
		*bitmaps = append(*bitmaps, postings[index].bitmap())
	}
	out := roar.OrMany(*bitmaps...)
	releaseBitmaps(bitmaps)
	return out
}
