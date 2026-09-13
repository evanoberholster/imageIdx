package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/roar"
)

// tinyCapacity is the maximum number of IDs stored inline in a tiny posting.
const tinyCapacity = 4

// tinyPosting stores up to tinyCapacity sorted IDs inline, avoiding a bitmap
// allocation for rare values.
type tinyPosting struct {
	n   uint8
	ids [tinyCapacity]uint64
}

func tinyContains(p *tinyPosting, id uint64) bool {
	for index := 0; index < int(p.n); index++ {
		if p.ids[index] == id {
			return true
		}
	}
	return false
}

// posting is the tagged posting representation: an inline container for up to
// four IDs, otherwise a roaring bitmap. The zero value is an empty posting.
type posting struct {
	tiny tinyPosting
	bit  roar.Bitmap
}

func (p *posting) isTiny() bool { return p.tiny.n != 0 }

func (p *posting) cardinality() uint64 {
	if p.isTiny() {
		return uint64(p.tiny.n)
	}
	return p.bit.Cardinality()
}

func (p *posting) contains(id uint64) bool {
	if p.isTiny() {
		return tinyContains(&p.tiny, id)
	}
	return p.bit.Contains(id)
}

func (p *posting) appendTo(dst []ImageID) []ImageID {
	if p.isTiny() {
		return append(dst, p.tiny.ids[:p.tiny.n]...)
	}
	return p.bit.AppendTo(dst)
}

func (p *posting) addTo(mutable *roar.MutableBitmap) {
	if p.isTiny() {
		mutable.AddMany(p.tiny.ids[:p.tiny.n])
		return
	}
	mutable.Or(p.bit)
}

// bitmap materializes the posting as a roaring bitmap. Tiny postings build a
// small bitmap; large postings return the shared immutable bitmap.
func (p *posting) bitmap() roar.Bitmap {
	if p.isTiny() {
		return roar.FromUnsorted(p.tiny.ids[:p.tiny.n])
	}
	return p.bit
}

// makePosting wraps a bitmap, demoting postings of at most tinyCapacity IDs to
// the inline form. The bitmap's IDs must be ascending.
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

// intersectPostings intersects effective postings. A tiny smallest operand is
// probed against the others; otherwise operands materialize and intersect with
// the roaring N-way kernel.
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

// intersectPostingsCardinality counts the intersection without materializing
// the result when possible.
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
// directly; all-large unions keep the roaring N-way kernel; mixed unions fold
// into a pooled accumulator and encode once, so no Freeze result aliases
// pooled scratch.
func unionPostings(postings []*posting) roar.Bitmap {
	switch len(postings) {
	case 0:
		return roar.Bitmap{}
	case 1:
		return postings[0].bitmap()
	}
	allTiny, total := true, 0
	hasTiny := false
	for index := range postings {
		if postings[index].isTiny() {
			hasTiny = true
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
	if !hasTiny {
		bitmaps := acquireBitmaps(len(postings))
		for index := range postings {
			*bitmaps = append(*bitmaps, postings[index].bit)
		}
		out := roar.OrMany(*bitmaps...)
		releaseBitmaps(bitmaps)
		return out
	}
	mutable := acquireMutable()
	ids := acquireIDs(total)
	for index := range postings {
		postings[index].addTo(mutable)
	}
	*ids = mutable.AppendTo((*ids)[:0])
	out := roar.FromUnsorted(*ids)
	releaseMutable(mutable)
	releaseIDs(ids)
	return out
}
