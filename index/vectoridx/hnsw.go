package vectoridx

import (
	"math"
	"sort"
	"sync/atomic"
)

// HNSW graph core over immutable snapshots. Read methods take no locks:
// they load link pointers atomically and filter phantoms via bounds checks,
// so concurrent writer appends never crash readers. Writer mutation helpers
// (prune, link) allocate new per-node layer sets and swap pointers.
//
// Edges store NodeIDs (never ordinals or pointers); ordinals resolve through
// the dense table (fast path) or sorted locator. Tombstoned nodes stay in the
// graph and remain traversable; they are only suppressed at result assembly.

// nodeLayers is one node's immutable neighbor set. Writers replace the whole
// pointer; readers hold the loaded pointer for the traversal.
type nodeLayers struct {
	layers [][]NodeID
}

// denseTable is an O(1) NodeID -> ordinal locator for dense ID ranges.
// Element access uses atomic funcs so writer fills race safely with lock-free
// readers; readers bounds-check ord against the snapshot count to filter
// phantoms (new IDs not yet visible to their snapshot).
type denseTable struct {
	min  NodeID
	vals []int32 // ord or -1
}

func denseGet(dt *denseTable, id NodeID) (int, bool) {
	if dt == nil {
		return 0, false
	}
	if id < dt.min {
		return 0, false
	}
	idx := uint64(id - dt.min)
	if idx >= uint64(len(dt.vals)) {
		return 0, false
	}
	ord := int(atomic.LoadInt32(&dt.vals[idx]))
	if ord < 0 {
		return 0, false
	}
	return ord, true
}

func levelMultiplier(m uint16) float64 {
	return 1.0 / math.Log(float64(m))
}

// locate resolves id to its physical ordinal with snapshot isolation: ord is
// valid only when ord < len(ids) and ids[ord] == id, filtering writer
// phantoms from in-place dense fills.
func (s *vectorSnapshot) locate(id NodeID) (ord int, ok bool) {
	if s.useDense && s.dense != nil {
		if o, found := denseGet(s.dense, id); found {
			if o < len(s.ids) && s.ids[o] == id {
				return o, true
			}
			return 0, false
		}
		// Dense miss may still exist when the table is disabled mid-range;
		// fall through to the sorted locator.
	}
	lo, hi := 0, len(s.sortedIDs)
	for lo < hi {
		mid := (lo + hi) >> 1
		if s.sortedIDs[mid] < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(s.sortedIDs) && s.sortedIDs[lo] == id {
		o := s.sortedOrd[lo]
		if o < len(s.ids) && s.ids[o] == id {
			return o, true
		}
	}
	return 0, false
}

// live reports the authoritative live condition: node exists in node
// metadata AND is not tombstoned.
func (s *vectorSnapshot) live(id NodeID) bool {
	if _, ok := s.locate(id); !ok {
		return false
	}
	return !s.tomb.Contains(uint64(id))
}

// vectorAt returns the raw vector slice for a physical ordinal.
// The slice aliases snapshot storage; callers must not retain it past the
// reader token release. Resurrected vectors may read fresh under concurrent
// updates (eventual consistency, never out of bounds).
func (s *vectorSnapshot) vectorAt(ord int) []byte {
	off := ord * s.vecBytes
	return s.vectors[off : off+s.vecBytes]
}

// rawNorm returns |raw| for cosine indexes and zero for other metrics. The
// value is computed once per query and for each stored vector on append/load,
// so the hot loop never recomputes a norm.
func (s *vectorSnapshot) rawNorm(raw []byte) float64 {
	if s.cfg.Metric != MetricCosine {
		return 0
	}
	return math.Sqrt(s.dot(raw, raw))
}

// appendVectorNorm records the norm for a vector just appended to candidates.
func (s *vectorSnapshot) appendVectorNorm(raw []byte) {
	if s.cfg.Metric == MetricCosine {
		s.norms = append(s.norms, s.rawNorm(raw))
	}
}

// normAt returns the cached norm for a stored ordinal, or zero for metrics
// that do not use norms.
func (s *vectorSnapshot) normAt(ord int) float64 {
	if s.cfg.Metric == MetricCosine {
		return s.norms[ord]
	}
	return 0
}

// distQuery computes the metric distance from query (with precomputed norm)
// to stored ordinal ord. Cosine reuses cached norms; other metrics call the
// pre-resolved raw kernel directly.
func (s *vectorSnapshot) distQuery(query []byte, queryNorm float64, ord int) float64 {
	if s.cfg.Metric == MetricCosine {
		return cosineFromDotNorms(s.dot(query, s.vectorAt(ord)), queryNorm, s.norms[ord])
	}
	return s.dist(query, s.vectorAt(ord))
}

// distOrds computes the metric distance between two stored ordinals. Cosine
// reuses both cached norms.
func (s *vectorSnapshot) distOrds(a, b int) float64 {
	if s.cfg.Metric == MetricCosine {
		return cosineFromDotNorms(s.dot(s.vectorAt(a), s.vectorAt(b)), s.norms[a], s.norms[b])
	}
	return s.dist(s.vectorAt(a), s.vectorAt(b))
}

// linkAt loads one node's immutable layer set (lock-free).
func (s *vectorSnapshot) linkAt(ord int) *nodeLayers {
	if ord < 0 || ord >= len(s.links) {
		return nil
	}
	return s.links[ord].Load()
}

// greedyStep moves from cur to its closest neighbor at level (including
// itself) by query distance, repeating until no improvement.
func (s *vectorSnapshot) greedyStep(cur, level int, query []byte, queryNorm float64) int {
	best := cur
	bestDist := s.distQuery(query, queryNorm, cur)
	for {
		improved := false
		g := s.linkAt(cur)
		if g == nil || level >= len(g.layers) {
			return best
		}
		for _, nb := range g.layers[level] {
			nOrd, ok := s.locate(nb)
			if !ok {
				continue
			}
			d := s.distQuery(query, queryNorm, nOrd)
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

// searchLayer runs best-first HNSW exploration at one level from entryOrd,
// returning up to ef closest entries in ctx-owned scratch.
func (s *vectorSnapshot) searchLayer(ctx *SearchContext, query []byte, queryNorm float64, entryOrd, ef, level int) []searchEntry {
	gen := ctx.gen
	visited := ctx.visited
	cand := &ctx.cand
	best := &ctx.best

	entryDist := s.distQuery(query, queryNorm, entryOrd)
	visited[entryOrd] = gen
	cand.push(searchEntry{ord: entryOrd, dist: entryDist})
	best.push(searchEntry{ord: entryOrd, dist: entryDist})

	for cand.len() != 0 {
		c := cand.pop()
		if best.len() >= ef && c.dist > best.peek().dist {
			break
		}
		g := s.linkAt(c.ord)
		if g == nil || level >= len(g.layers) {
			continue
		}
		for _, nb := range g.layers[level] {
			nOrd, ok := s.locate(nb)
			if !ok {
				continue
			}
			if visited[nOrd] == gen {
				continue
			}
			visited[nOrd] = gen
			d := s.distQuery(query, queryNorm, nOrd)
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

// layer0Search descends from the entry to layer 0 then explores with budget
// ef, returning raw entries (unfiltered, may include tombstones).
func (s *vectorSnapshot) layer0Search(ctx *SearchContext, query []byte, ef int) []searchEntry {
	entryOrd, ok := s.locate(s.entry)
	if !ok {
		return nil
	}
	cur := entryOrd
	for l := s.maxLevel; l > 0; l-- {
		cur = s.greedyStep(cur, l, query, ctx.queryNorm)
	}
	return s.searchLayer(ctx, query, ctx.queryNorm, cur, ef, 0)
}

// effectiveEf computes the layer-0 exploration budget for k, applying the
// dynamic-ef policy (Weaviate dynamicEf analogue) and filtered oversampling.
func (s *vectorSnapshot) effectiveEf(k int, filter CandidateFilter) int {
	ef := int(s.cfg.EfSearch)
	if s.cfg.DynamicEfFactor > 0 {
		dyn := int(uint64(k) * uint64(s.cfg.DynamicEfFactor))
		if dyn < int(s.cfg.DynamicEfMin) {
			dyn = int(s.cfg.DynamicEfMin)
		}
		if dyn > int(s.cfg.DynamicEfMax) {
			dyn = int(s.cfg.DynamicEfMax)
		}
		ef = dyn
	}
	if ef < k {
		ef = k
	}
	if filter != nil {
		if want := k * int(s.cfg.OversampleFactor); want > ef {
			ef = want
		}
	}
	if ef > len(s.ids) {
		ef = len(s.ids)
	}
	if ef < 1 {
		ef = 1
	}
	return ef
}

// assemble converts raw layer-0 entries into the final top-k: skip
// tombstones, apply the candidate filter, sort, and encode scores into buf.
// With FilterAware enabled the result heap favors filter-passing nodes while
// traversal stays unconstrained (ACORN-lite): callers oversample ef, so this
// stays a post-filter with iterative expansion in the caller.
func (s *vectorSnapshot) assemble(entries []searchEntry, filter CandidateFilter, k int, buf []Result, querySelfDot float64) []Result {
	if k <= 0 || len(entries) == 0 {
		return buf[:0]
	}
	filtered := entries[:0]
	for _, e := range entries {
		if e.ord < 0 || e.ord >= len(s.ids) {
			continue
		}
		id := s.ids[e.ord]
		if s.tomb.Contains(uint64(id)) {
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
	sortAssembled(filtered, s.ids)
	if len(filtered) > k {
		filtered = filtered[:k]
	}
	metric := s.cfg.Metric
	out := buf[:0]
	for _, e := range filtered {
		out = append(out, Result{ValueID: widenedID(s.ids[e.ord]), Score: encodeScoreForMetric(metric, e.dist, querySelfDot)})
	}
	return out
}

// sortAssembled orders filtered candidates by (distance, NodeID) to match
// the exact-scan oracle's tie-break.
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

// exactSearchInto is the pooled exact reference: full scan over live nodes
// honoring tombstones and filter, top-k selected through the context's
// reusable heap and encoded into buf. Steady-state it allocates nothing.
func (s *vectorSnapshot) exactSearchInto(ctx *SearchContext, query []byte, filter CandidateFilter, k int, buf []Result) []Result {
	if k <= 0 || len(s.ids) == 0 {
		return buf[:0]
	}
	ctx.cand.reset()
	ctx.best.reset()
	best := &ctx.best
	for ord, id := range s.ids {
		if s.tomb.Contains(uint64(id)) {
			continue
		}
		if filter != nil && !filter(id) {
			continue
		}
		d := s.distQuery(query, ctx.queryNorm, ord)
		if best.len() < k {
			best.push(searchEntry{ord: ord, dist: d})
			continue
		}
		top := best.peek()
		if d < top.dist || (d == top.dist && id < s.ids[top.ord]) {
			best.pop()
			best.push(searchEntry{ord: ord, dist: d})
		}
	}
	all := append(ctx.layer[:0], best.items...)
	ctx.layer = all
	sortAssembled(all, s.ids)
	metric := s.cfg.Metric
	var querySelfDot float64
	if metric == MetricDot {
		querySelfDot = dotSelfBytes(s.cfg, query)
	}
	out := buf[:0]
	for _, e := range all {
		out = append(out, Result{ValueID: widenedID(s.ids[e.ord]), Score: encodeScoreForMetric(metric, e.dist, querySelfDot)})
	}
	return out
}

// exactSearch is the scalar exact reference for the index metric: full scan
// over live nodes honoring tombstones and filter, sorted, truncated to k.
func (s *vectorSnapshot) exactSearch(query []byte, filter CandidateFilter, k int) []Result {
	if k <= 0 || len(s.ids) == 0 {
		return nil
	}
	type scored struct {
		id   NodeID
		dist float64
	}
	queryNorm := s.rawNorm(query)
	all := make([]scored, 0, len(s.ids))
	for ord, id := range s.ids {
		if s.tomb.Contains(uint64(id)) {
			continue
		}
		if filter != nil && !filter(id) {
			continue
		}
		all = append(all, scored{id: id, dist: s.distQuery(query, queryNorm, ord)})
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
	metric := s.cfg.Metric
	var querySelfDot float64
	if metric == MetricDot {
		querySelfDot = dotSelfBytes(s.cfg, query)
	}
	out := make([]Result, len(all))
	for i, sc := range all {
		out[i] = Result{ValueID: widenedID(sc.id), Score: encodeScoreForMetric(metric, sc.dist, querySelfDot)}
	}
	return out
}

// searchWithHint runs the full ANN path with an optional filter cardinality
// hint for the cost-based flat fallback. flatSmall reports whether the flat
// path was taken.
func (s *vectorSnapshot) searchWithHint(ctx *SearchContext, query []byte, filter CandidateFilter, k int, filterHint int) ([]Result, bool) {
	if k > len(s.ids) {
		k = len(s.ids)
	}
	ctx.queryNorm = s.rawNorm(query)
	live := len(s.ids) - len(s.tombSorted)
	if s.cfg.FlatThreshold >= 0 && live <= s.cfg.FlatThreshold {
		out := s.exactSearchInto(ctx, query, filter, k, ctx.output[:0])
		ctx.output = out
		return out, true
	}
	if filter != nil && filterHint >= 0 && s.cfg.FlatSearchCutoff >= 0 && filterHint <= s.cfg.FlatSearchCutoff {
		out := s.exactSearchInto(ctx, query, filter, k, ctx.output[:0])
		ctx.output = out
		return out, true
	}
	var querySelfDot float64
	if s.cfg.Metric == MetricDot {
		querySelfDot = dotSelfBytes(s.cfg, query)
	}
	ef := s.effectiveEf(k, filter)
	ctx.resetForNodes(len(s.ids), s.vecBytes)
	entries := s.layer0Search(ctx, query, ef)
	results := s.assemble(entries, filter, k, ctx.output[:0], querySelfDot)
	ctx.output = results
	if filter != nil && len(results) < k {
		if ef < len(s.ids) {
			ef2 := ef * 2
			if ef2 > len(s.ids) {
				ef2 = len(s.ids)
			}
			if ef2 > ef {
				ctx.resetForNodes(len(s.ids), s.vecBytes)
				entries = s.layer0Search(ctx, query, ef2)
				expanded := s.assemble(entries, filter, k, ctx.output[:0], querySelfDot)
				ctx.output = expanded
				if len(expanded) > len(results) {
					results = expanded
				}
				ef = ef2
			}
		}
		if len(results) < k {
			available := 0
			cutoff := s.cfg.FlatSearchCutoff
			for _, id := range s.sortedIDs {
				if s.tomb.Contains(uint64(id)) {
					continue
				}
				if !filter(id) {
					continue
				}
				available++
				if cutoff >= 0 && available > cutoff && len(results) >= k {
					break
				}
			}
			// Dense snapshots may leave sortedIDs stale; count via ids.
			if len(s.sortedIDs) == 0 {
				for _, id := range s.ids {
					if s.tomb.Contains(uint64(id)) {
						continue
					}
					if filter != nil && !filter(id) {
						continue
					}
					available++
					if cutoff >= 0 && available > cutoff && len(results) >= k {
						break
					}
				}
			}
			if available <= k || len(results) < k {
				// Allocating fallback: results aliases ctx scratch, so the
				// exact scan takes its own storage here.
				exact := s.exactSearch(query, filter, k)
				if len(exact) > len(results) {
					return exact, true
				}
				if available <= k {
					return exact, true
				}
			}
		}
	}
	return results, false
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
