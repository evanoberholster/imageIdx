package vectoridx

import (
	"math"
	"sort"
	"sync"
)

// Search scratch structures. The pooled SearchContext owns every temporary
// heap, tag array, and buffer so steady-state search allocates nothing
// except the caller-owned result copy (VectorIdx.MD sections 18-19, 29).

// searchEntry is one candidate: a physical ordinal plus its squared-L2
// distance to the query. Ordinals (not NodeIDs) index visited tags, keeping
// scratch memory proportional to node count rather than max(NodeID).
type searchEntry struct {
	ord  int
	dist float64
}

// minHeap is a binary min-heap over dist (candidate frontier).
type minHeap struct {
	items []searchEntry
}

func (h *minHeap) reset() { h.items = h.items[:0] }

func (h *minHeap) push(e searchEntry) {
	items := append(h.items, e)
	i := len(items) - 1
	for i > 0 {
		parent := (i - 1) >> 1
		if items[parent].dist <= items[i].dist {
			break
		}
		items[parent], items[i] = items[i], items[parent]
		i = parent
	}
	h.items = items
}

func (h *minHeap) pop() searchEntry {
	items := h.items
	top := items[0]
	last := items[len(items)-1]
	items = items[:len(items)-1]
	if len(items) != 0 {
		i := 0
		for {
			left := (i << 1) + 1
			if left >= len(items) {
				break
			}
			smallest := left
			if right := left + 1; right < len(items) && items[right].dist < items[left].dist {
				smallest = right
			}
			if items[smallest].dist >= last.dist {
				break
			}
			items[i] = items[smallest]
			i = smallest
		}
		items[i] = last
	}
	h.items = items
	return top
}

func (h *minHeap) peek() searchEntry { return h.items[0] }
func (h *minHeap) len() int          { return len(h.items) }

// maxHeap is a binary max-heap over dist (current best-ef results).
type maxHeap struct {
	items []searchEntry
}

func (h *maxHeap) reset() { h.items = h.items[:0] }

func (h *maxHeap) push(e searchEntry) {
	items := append(h.items, e)
	i := len(items) - 1
	for i > 0 {
		parent := (i - 1) >> 1
		if items[parent].dist >= items[i].dist {
			break
		}
		items[parent], items[i] = items[i], items[parent]
		i = parent
	}
	h.items = items
}

func (h *maxHeap) pop() searchEntry {
	items := h.items
	top := items[0]
	last := items[len(items)-1]
	items = items[:len(items)-1]
	if len(items) != 0 {
		i := 0
		for {
			left := (i << 1) + 1
			if left >= len(items) {
				break
			}
			largest := left
			if right := left + 1; right < len(items) && items[right].dist > items[left].dist {
				largest = right
			}
			if items[largest].dist <= last.dist {
				break
			}
			items[i] = items[largest]
			i = largest
		}
		items[i] = last
	}
	h.items = items
	return top
}

func (h *maxHeap) peek() searchEntry { return h.items[0] }
func (h *maxHeap) len() int          { return len(h.items) }

// SearchContext owns complete per-query scratch state. It is pooled and must
// never escape to callers: SearchInto copies final Results out before Put.
type SearchContext struct {
	cand    minHeap
	best    maxHeap
	visited []uint32
	gen     uint32
	// queryBuf holds the encoded raw query vector for the hot loop.
	queryBuf []byte
	// layer reuses the per-level candidate buffer across layer searches.
	layer []searchEntry
	// output reuses the result-assembly buffer; callers copy out before Put.
	output []Result
}

func (s *SearchContext) resetForNodes(nodeCount, vecBytes int) {
	s.cand.reset()
	s.best.reset()
	s.layer = s.layer[:0]
	s.output = s.output[:0]
	if cap(s.visited) < nodeCount {
		s.visited = make([]uint32, nodeCount)
		s.gen = 0
	} else {
		s.visited = s.visited[:nodeCount]
	}
	s.gen++
	if s.gen == 0 || s.gen == math.MaxUint32 {
		// Generation rollover: clear tags and restart at 1.
		for i := range s.visited {
			s.visited[i] = 0
		}
		s.gen = 1
	}
	if cap(s.queryBuf) < vecBytes {
		s.queryBuf = make([]byte, 0, vecBytes)
	}
}

var searchPool sync.Pool // of *SearchContext

func acquireSearchContext() *SearchContext {
	if v := searchPool.Get(); v != nil {
		return v.(*SearchContext)
	}
	return &SearchContext{}
}

func releaseSearchContext(s *SearchContext) {
	// Keep backing arrays for reuse but drop logical lengths.
	s.cand.reset()
	s.best.reset()
	s.layer = s.layer[:0]
	s.output = s.output[:0]
	searchPool.Put(s)
}

// PrewarmSearchContexts places n ready contexts into the pool so the first
// queries avoid allocation. It is optional; search grows the pool on demand.
func PrewarmSearchContexts(n int) {
	for range n {
		releaseSearchContext(&SearchContext{})
	}
}

// lessEntry orders candidates by distance, breaking ties by ordinal for
// deterministic results.
func lessEntry(a, b searchEntry) bool {
	if a.dist != b.dist {
		return a.dist < b.dist
	}
	return a.ord < b.ord
}

// sortEntries sorts candidates for result assembly. Small slices use
// insertion sort, which is allocation-free: sort.Slice would box the slice
// header into an interface on every query (several steady-state allocs for
// a hot-path sort). Above the threshold, O(n^2) would dominate distance
// work, so the standard library sort is used; very large ef values are
// rare and already pay for proportionally more distance evaluations.
func sortEntries(entries []searchEntry) {
	if len(entries) < 128 {
		for i := 1; i < len(entries); i++ {
			for j := i; j > 0 && lessEntry(entries[j], entries[j-1]); j-- {
				entries[j-1], entries[j] = entries[j], entries[j-1]
			}
		}
		return
	}
	sort.Slice(entries, func(i, j int) bool { return lessEntry(entries[i], entries[j]) })
}

// selectClosest returns the m closest candidates for the writer (insertion)
// path.
func selectClosest(cands []searchEntry, m int) []searchEntry {
	if len(cands) > m {
		sortEntries(cands)
		cands = cands[:m]
	}
	return cands
}

// selectHeuristic implements the hnswlib diversity heuristic: candidates
// sorted by distance to the query are kept when closer to the query than to
// any already-selected neighbor, spreading edges across directions instead
// of collapsing onto one cluster. distFn measures raw-vector distance;
// vectorAt resolves ordinals. It returns at most m entries.
func selectHeuristic(cands []searchEntry, m int, query []byte, s *vectorSnapshot) []searchEntry {
	if len(cands) <= m {
		return cands
	}
	sortEntries(cands)
	selected := cands[:0]
	for _, c := range cands {
		if len(selected) >= m {
			break
		}
		cVec := s.vectorAt(c.ord)
		keep := true
		for _, kept := range selected {
			d := s.dist(cVec, s.vectorAt(kept.ord))
			if d < c.dist {
				keep = false
				break
			}
		}
		_ = query
		if keep {
			selected = append(selected, c)
		}
	}
	// If diversity pruned too aggressively, backfill closest remainder.
	if len(selected) < m {
		inSel := make(map[int]bool, len(selected))
		for _, c := range selected {
			inSel[c.ord] = true
		}
		for _, c := range cands {
			if len(selected) >= m {
				break
			}
			if !inSel[c.ord] {
				selected = append(selected, c)
			}
		}
	}
	return selected
}
