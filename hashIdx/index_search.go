package hashidx

import (
	"math/bits"
	"slices"
)

// Workspace holds reusable search scratch. A Workspace is not safe for
// concurrent use; one workspace belongs to one goroutine at a time.
type Workspace struct {
	heap []Result
}

// SearchPHashRadius appends every record whose exact 64-bit Hamming distance
// from query is at most maxDistance to dst. Matches are appended in physical
// ordinal order. A nil candidates set disables filtering. With sufficient dst
// capacity the search allocates nothing.
func (i *Index) SearchPHashRadius(dst []Result, query uint64, maxDistance uint8, candidates CandidateSet) []Result {
	token := i.readers.Acquire()
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			dst = searchPHashRadius(dst, view, query, maxDistance, candidates)
		}
	}
	i.readers.Release(token)
	return dst
}

// SearchPDQRadius appends every record whose exact 256-bit Hamming distance
// from query is at most maxDistance to dst. Matches are appended in physical
// ordinal order. A nil candidates set disables filtering. With sufficient dst
// capacity the search allocates nothing.
func (i *Index) SearchPDQRadius(dst []Result, query PDQHash, maxDistance uint16, candidates CandidateSet) []Result {
	token := i.readers.Acquire()
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			dst = searchPDQRadius(dst, view, query, maxDistance, candidates)
		}
	}
	i.readers.Release(token)
	return dst
}

// SearchPHashTopK appends up to k nearest records, ordered by ascending
// Hamming distance with ImageID as a deterministic tie-break. The bounded
// max-heap reuses ws scratch and, when ws is nil, allocates one internally.
func (i *Index) SearchPHashTopK(dst []Result, query uint64, k int, candidates CandidateSet, ws *Workspace) []Result {
	if k <= 0 {
		return dst
	}
	if ws == nil {
		ws = &Workspace{}
	}
	ws.heap = ws.heap[:0]
	token := i.readers.Acquire()
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			searchPHashTopK(&ws.heap, view, query, k, candidates)
		}
	}
	i.readers.Release(token)
	return appendSorted(dst, ws.heap)
}

// SearchPDQTopK appends up to k nearest records, ordered by ascending
// Hamming distance with ImageID as a deterministic tie-break. The bounded
// max-heap reuses ws scratch and, when ws is nil, allocates one internally.
func (i *Index) SearchPDQTopK(dst []Result, query PDQHash, k int, candidates CandidateSet, ws *Workspace) []Result {
	if k <= 0 {
		return dst
	}
	if ws == nil {
		ws = &Workspace{}
	}
	ws.heap = ws.heap[:0]
	token := i.readers.Acquire()
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			searchPDQTopK(&ws.heap, view, query, k, candidates)
		}
	}
	i.readers.Release(token)
	return appendSorted(dst, ws.heap)
}

// appendSorted sorts the bounded heap by ascending distance and ImageID and
// appends it to dst.
func appendSorted(dst []Result, heap []Result) []Result {
	slices.SortFunc(heap, compareResult)
	return append(dst, heap...)
}

func compareResult(a, b Result) int {
	switch {
	case a.Distance < b.Distance:
		return -1
	case a.Distance > b.Distance:
		return 1
	case a.ImageID < b.ImageID:
		return -1
	case a.ImageID > b.ImageID:
		return 1
	default:
		return 0
	}
}

// searchPHashRadius scans sealed pages and then the active prefix, computing
// the distance before consulting candidates.
func searchPHashRadius(dst []Result, view *indexView, query uint64, maxDistance uint8, candidates CandidateSet) []Result {
	maxDist := int(maxDistance)
	for pageIndex := range view.pages {
		page := &view.pages[pageIndex]
		dst = phashRadiusPlane(dst, page.imageIDs, page.phash64, query, maxDist, candidates)
	}
	if view.active != nil {
		dst = phashRadiusPlane(dst, view.active.imageIDs[:view.activeCount],
			view.active.phash64[:view.activeCount], query, maxDist, candidates)
	}
	return dst
}

func phashRadiusPlane(dst []Result, ids []ImageID, hashes []uint64, query uint64, maxDist int, candidates CandidateSet) []Result {
	for k := range hashes {
		distance := bits.OnesCount64(query ^ hashes[k])
		if distance > maxDist {
			continue
		}
		id := ids[k]
		if candidates != nil && !candidates.Contains(id) {
			continue
		}
		dst = append(dst, Result{ImageID: id, Distance: uint16(distance)})
	}
	return dst
}

// searchPDQRadius scans with the scalar early-exit kernel: the running
// distance is compared against maxDistance after every word.
func searchPDQRadius(dst []Result, view *indexView, query PDQHash, maxDistance uint16, candidates CandidateSet) []Result {
	maxDist := int(maxDistance)
	for pageIndex := range view.pages {
		page := &view.pages[pageIndex]
		dst = pdqRadiusPlane(dst, page.imageIDs, page.pdq, query, maxDist, candidates)
	}
	if view.active != nil {
		active := view.active
		count := view.activeCount
		dst = pdqRadiusPlane(dst, active.imageIDs[:count], [4][]uint64{
			active.pdq[0][:count], active.pdq[1][:count],
			active.pdq[2][:count], active.pdq[3][:count],
		}, query, maxDist, candidates)
	}
	return dst
}

func pdqRadiusPlane(dst []Result, ids []ImageID, pdq [4][]uint64, query PDQHash, maxDist int, candidates CandidateSet) []Result {
	q0, q1, q2, q3 := query[0], query[1], query[2], query[3]
	p0, p1, p2, p3 := pdq[0], pdq[1], pdq[2], pdq[3]
	for k := range p0 {
		distance := bits.OnesCount64(q0 ^ p0[k])
		if distance > maxDist {
			continue
		}
		distance += bits.OnesCount64(q1 ^ p1[k])
		if distance > maxDist {
			continue
		}
		distance += bits.OnesCount64(q2 ^ p2[k])
		if distance > maxDist {
			continue
		}
		distance += bits.OnesCount64(q3 ^ p3[k])
		if distance > maxDist {
			continue
		}
		id := ids[k]
		if candidates != nil && !candidates.Contains(id) {
			continue
		}
		dst = append(dst, Result{ImageID: id, Distance: uint16(distance)})
	}
	return dst
}

// searchPHashTopK maintains a bounded max-heap whose root is the current
// worst result; a better candidate replaces the root.
func searchPHashTopK(heap *[]Result, view *indexView, query uint64, k int, candidates CandidateSet) {
	for pageIndex := range view.pages {
		page := &view.pages[pageIndex]
		phashTopKPlane(heap, page.imageIDs, page.phash64, query, k, candidates)
	}
	if view.active != nil {
		phashTopKPlane(heap, view.active.imageIDs[:view.activeCount],
			view.active.phash64[:view.activeCount], query, k, candidates)
	}
}

func phashTopKPlane(heap *[]Result, ids []ImageID, hashes []uint64, query uint64, k int, candidates CandidateSet) {
	for j := range hashes {
		id := ids[j]
		if candidates != nil && !candidates.Contains(id) {
			continue
		}
		result := Result{ImageID: id, Distance: uint16(bits.OnesCount64(query ^ hashes[j]))}
		if len(*heap) < k {
			heapPush(heap, result)
			continue
		}
		if compareResult(result, (*heap)[0]) < 0 {
			heapReplaceRoot(heap, result)
		}
	}
}

// searchPDQTopK is searchPHashTopK with the scalar PDQ kernel and no early
// exit (Top-K cannot reject on a partial distance).
func searchPDQTopK(heap *[]Result, view *indexView, query PDQHash, k int, candidates CandidateSet) {
	for pageIndex := range view.pages {
		page := &view.pages[pageIndex]
		pdqTopKPlane(heap, page.imageIDs, page.pdq, query, k, candidates)
	}
	if view.active != nil {
		active := view.active
		count := view.activeCount
		pdqTopKPlane(heap, active.imageIDs[:count], [4][]uint64{
			active.pdq[0][:count], active.pdq[1][:count],
			active.pdq[2][:count], active.pdq[3][:count],
		}, query, k, candidates)
	}
}

func pdqTopKPlane(heap *[]Result, ids []ImageID, pdq [4][]uint64, query PDQHash, k int, candidates CandidateSet) {
	q0, q1, q2, q3 := query[0], query[1], query[2], query[3]
	p0, p1, p2, p3 := pdq[0], pdq[1], pdq[2], pdq[3]
	for j := range p0 {
		id := ids[j]
		if candidates != nil && !candidates.Contains(id) {
			continue
		}
		distance := bits.OnesCount64(q0^p0[j]) +
			bits.OnesCount64(q1^p1[j]) +
			bits.OnesCount64(q2^p2[j]) +
			bits.OnesCount64(q3^p3[j])
		result := Result{ImageID: id, Distance: uint16(distance)}
		if len(*heap) < k {
			heapPush(heap, result)
			continue
		}
		if compareResult(result, (*heap)[0]) < 0 {
			heapReplaceRoot(heap, result)
		}
	}
}

// heapPush appends result and restores the max-heap property.
func heapPush(heap *[]Result, result Result) {
	h := append(*heap, result)
	index := len(h) - 1
	for index > 0 {
		parent := (index - 1) / 2
		if compareResult(h[parent], h[index]) >= 0 {
			break
		}
		h[parent], h[index] = h[index], h[parent]
		index = parent
	}
	*heap = h
}

// heapReplaceRoot replaces the worst result and sifts the new result down.
func heapReplaceRoot(heap *[]Result, result Result) {
	h := *heap
	h[0] = result
	n := len(h)
	index := 0
	for {
		left := 2*index + 1
		if left >= n {
			break
		}
		largest := left
		if right := left + 1; right < n && compareResult(h[right], h[left]) > 0 {
			largest = right
		}
		if compareResult(h[index], h[largest]) >= 0 {
			break
		}
		h[index], h[largest] = h[largest], h[index]
		index = largest
	}
}
