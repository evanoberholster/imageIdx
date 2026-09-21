package vectoridx

import (
	"math"
	"sort"
)

// HNSW graph core. All functions assume the caller holds the appropriate
// lock (writer Lock for insertion, reader RLock for traversal) so ordinals,
// vectors, and links stay stable for the call duration.
//
// Edges store NodeIDs (never ordinals or pointers); ordinals resolve through
// the sorted locator. Tombstoned nodes stay in the graph and remain
// traversable; they are only suppressed at result assembly.

// levelMultiplier returns 1/ln(M), the standard HNSW normalization.
func levelMultiplier(m uint16) float64 {
	return 1.0 / math.Log(float64(m))
}

// locateLocked resolves id to its physical ordinal via binary search over
// the sorted locator. ok=false when the node never existed or was compacted
// away. Presence (exists AND not tombstoned) is a separate check.
func (v *VectorIdx) locateLocked(id NodeID) (ord int, ok bool) {
	lo, hi := 0, len(v.sortedIDs)
	for lo < hi {
		mid := (lo + hi) >> 1
		if v.sortedIDs[mid] < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(v.sortedIDs) && v.sortedIDs[lo] == id {
		return v.sortedOrd[lo], true
	}
	return 0, false
}

// liveLocked reports the authoritative live condition: node exists in node
// metadata AND is not tombstoned (VectorIdx.MD section 28).
func (v *VectorIdx) liveLocked(id NodeID) bool {
	if _, ok := v.locateLocked(id); !ok {
		return false
	}
	_, tombstoned := v.tombstones[id]
	return !tombstoned
}

// vectorAtLocked returns the raw vector slice for a physical ordinal.
// The slice aliases index storage; callers must not retain it past unlock.
func (v *VectorIdx) vectorAtLocked(ord int) []byte {
	off := ord * v.vecBytes
	return v.vectors[off : off+v.vecBytes]
}

// greedyStep moves from cur to its closest neighbor at level (including
// itself) by query distance, repeating until no improvement. It returns the
// closest ordinal found.
func (v *VectorIdx) greedyStepLocked(cur, level int, query []byte) int {
	best := cur
	bestDist := v.dist(query, v.vectorAtLocked(cur))
	for {
		improved := false
		for _, nb := range v.links[cur][level] {
			nOrd, ok := v.locateLocked(nb)
			if !ok {
				continue
			}
			d := v.dist(query, v.vectorAtLocked(nOrd))
			if d < bestDist {
				bestDist = d
				best = nOrd
				improved = true
			}
		}
		if !improved {
			return best
		}
		cur = best
	}
}

// searchLayerLocked runs best-first HNSW exploration at one level from
// entryOrd, returning up to ef closest entries in ctx-owned scratch. The
// returned slice aliases ctx.layer and is valid only until the next
// searchLayerLocked call on the same context; callers consume it
// synchronously (copying the NodeIDs they need).
func (v *VectorIdx) searchLayerLocked(ctx *SearchContext, query []byte, entryOrd, ef, level int) []searchEntry {
	gen := ctx.gen
	visited := ctx.visited
	cand := &ctx.cand
	best := &ctx.best

	entryDist := v.dist(query, v.vectorAtLocked(entryOrd))
	visited[entryOrd] = gen
	cand.push(searchEntry{ord: entryOrd, dist: entryDist})
	best.push(searchEntry{ord: entryOrd, dist: entryDist})

	for cand.len() != 0 {
		c := cand.pop()
		if best.len() >= ef && c.dist > best.peek().dist {
			break
		}
		for _, nb := range v.links[c.ord][level] {
			nOrd, ok := v.locateLocked(nb)
			if !ok {
				continue
			}
			if visited[nOrd] == gen {
				continue
			}
			visited[nOrd] = gen
			d := v.dist(query, v.vectorAtLocked(nOrd))
			if best.len() < ef || d < best.peek().dist {
				cand.push(searchEntry{ord: nOrd, dist: d})
				best.push(searchEntry{ord: nOrd, dist: d})
				if best.len() > ef {
					best.pop()
				}
			}
		}
	}
	out := append(ctx.layer[:0], best.items...)
	ctx.layer = out
	return out
}

// insertLocked wires a new node (already appended at ordinal newOrd with its
// vector stored) into levels 0..level. It runs before entry/maxLevel update
// so the new node is invisible to its own neighbor search.
func (v *VectorIdx) insertLocked(ctx *SearchContext, query []byte, newOrd, newLevel int, newID NodeID) {
	if len(v.ids) == 1 {
		// First node: links already allocated empty; entry set by caller.
		return
	}
	entryOrd, ok := v.locateLocked(v.entry)
	if !ok {
		return
	}
	cur := entryOrd
	for l := v.maxLevel; l > newLevel; l-- {
		cur = v.greedyStepLocked(cur, l, query)
	}
	for l := min(v.maxLevel, newLevel); l >= 0; l-- {
		ef := int(v.cfg.EfConstruction)
		cands := v.searchLayerLocked(ctx, query, cur, ef, l)
		m := int(v.cfg.M)
		if l == 0 {
			m = int(v.cfg.M0)
		}
		selected := selectClosest(cands, m)
		// Forward links from the new node.
		v.links[newOrd][l] = v.links[newOrd][l][:0]
		for _, s := range selected {
			v.links[newOrd][l] = append(v.links[newOrd][l], v.ids[s.ord])
		}
		// Reciprocal links with closest-M pruning.
		for _, s := range selected {
			nb := s.ord
			nbLevel := v.levels[nb]
			if l > nbLevel {
				continue
			}
			v.links[nb][l] = append(v.links[nb][l], newID)
			if len(v.links[nb][l]) > m {
				v.pruneNeighborsLocked(nb, l, m)
			}
		}
		if len(selected) != 0 {
			cur = selected[0].ord
		}
	}
}

// pruneNeighborsLocked keeps the m closest neighbors of ord at level by
// current distance. It allocates one small slice per prune; insertion is
// writer-serialized and off the search hot path.
func (v *VectorIdx) pruneNeighborsLocked(ord, level, m int) {
	nbs := v.links[ord][level]
	type nd struct {
		id   NodeID
		dist float64
	}
	scored := make([]nd, 0, len(nbs))
	base := v.vectorAtLocked(ord)
	for _, nb := range nbs {
		nOrd, ok := v.locateLocked(nb)
		if !ok {
			continue
		}
		scored = append(scored, nd{id: nb, dist: v.dist(base, v.vectorAtLocked(nOrd))})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].dist != scored[j].dist {
			return scored[i].dist < scored[j].dist
		}
		return scored[i].id < scored[j].id
	})
	if len(scored) > m {
		scored = scored[:m]
	}
	kept := v.links[ord][level][:0]
	for _, s := range scored {
		kept = append(kept, s.id)
	}
	v.links[ord][level] = kept
}

// layer0SearchLocked descends from the entry to layer 0 then explores with
// budget ef, returning raw entries (unfiltered, may include tombstones).
func (v *VectorIdx) layer0SearchLocked(ctx *SearchContext, query []byte, ef int) []searchEntry {
	entryOrd, ok := v.locateLocked(v.entry)
	if !ok {
		return nil
	}
	cur := entryOrd
	for l := v.maxLevel; l > 0; l-- {
		cur = v.greedyStepLocked(cur, l, query)
	}
	return v.searchLayerLocked(ctx, query, cur, ef, 0)
}

// assembleLocked converts raw layer-0 entries into the final top-k: skip
// tombstones, apply the candidate filter, sort, and encode scores into buf.
// It never mutates traversal semantics for filtering (VectorIdx.MD section 20).
func (v *VectorIdx) assembleLocked(entries []searchEntry, filter CandidateFilter, k int, buf []Result) []Result {
	if k <= 0 || len(entries) == 0 {
		return buf[:0]
	}
	filtered := entries[:0]
	for _, e := range entries {
		id := v.ids[e.ord]
		if _, tombstoned := v.tombstones[id]; tombstoned {
			continue
		}
		if filter != nil && !filter(id) {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		return buf[:0]
	}
	sortAssembled(filtered, v.ids)
	if len(filtered) > k {
		filtered = filtered[:k]
	}
	out := buf[:0]
	for _, e := range filtered {
		out = append(out, Result{NodeID: v.ids[e.ord], Score: EncodeScore(e.dist)})
	}
	return out
}

// sortAssembled orders filtered candidates by (distance, NodeID) to match
// the exact-scan oracle's tie-break. Small slices use allocation-free
// insertion sort (sort.Slice would box the slice header per query); large
// ef values fall back to the standard sort.
func sortAssembled(entries []searchEntry, ids []NodeID) {
	if len(entries) < 128 {
		for i := 1; i < len(entries); i++ {
			for j := i; j > 0; j-- {
				a, b := entries[j], entries[j-1]
				if a.dist > b.dist || (a.dist == b.dist && ids[a.ord] >= ids[b.ord]) {
					break
				}
				entries[j-1], entries[j] = a, b
			}
		}
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].dist != entries[j].dist {
			return entries[i].dist < entries[j].dist
		}
		return ids[entries[i].ord] < ids[entries[j].ord]
	})
}

// exactSearchLocked is the scalar exact-L2 reference: full scan over live
// nodes honoring tombstones and filter, sorted, truncated to k. It is the
// recall oracle and the filtered-search fallback when ANN coverage is thin.
func (v *VectorIdx) exactSearchLocked(query []byte, filter CandidateFilter, k int) []Result {
	if k <= 0 || len(v.ids) == 0 {
		return nil
	}
	type scored struct {
		id   NodeID
		dist float64
	}
	all := make([]scored, 0, len(v.ids))
	for ord, id := range v.ids {
		if _, tombstoned := v.tombstones[id]; tombstoned {
			continue
		}
		if filter != nil && !filter(id) {
			continue
		}
		all = append(all, scored{id: id, dist: v.dist(query, v.vectorAtLocked(ord))})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].dist != all[j].dist {
			return all[i].dist < all[j].dist
		}
		return all[i].id < all[j].id
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([]Result, len(all))
	for i, s := range all {
		out[i] = Result{NodeID: s.id, Score: EncodeScore(s.dist)}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
