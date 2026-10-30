package vectoridx

import (
	"context"
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
	chunk := s.vectors[ord>>vectorChunkShift]
	off := (ord & (vectorChunkVectors - 1)) * s.vecBytes
	return chunk[off : off+s.vecBytes]
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

// appendVectorNorm records the norm for the vector just appended to ids.
func (s *vectorSnapshot) appendVectorNorm(raw []byte) {
	if s.cfg.Metric != MetricCosine {
		return
	}
	ord := len(s.ids) - 1
	s.setVectorNorm(ord, s.rawNorm(raw))
}

// setVectorNorm records a cached norm with chunk-level copy-on-write.
func (s *vectorSnapshot) setVectorNorm(ord int, norm float64) {
	ci := ord >> vectorChunkShift
	if ci == len(s.normChunks) {
		s.normChunks = append(s.normChunks, make([]float64, vectorChunkVectors))
	} else if ci > len(s.normChunks) {
		return
	}
	s.normChunks[ci][ord&(vectorChunkVectors-1)] = norm
}

// replaceVectorNorm updates an existing cached norm with copy-on-write.
func (s *vectorSnapshot) replaceVectorNorm(ord int, norm float64) {
	ci := ord >> vectorChunkShift
	if ci >= len(s.normChunks) {
		s.setVectorNorm(ord, norm)
		return
	}
	old := s.normChunks[ci]
	chunk := make([]float64, len(old))
	copy(chunk, old)
	chunk[ord&(vectorChunkVectors-1)] = norm
	dir := make([][]float64, len(s.normChunks))
	copy(dir, s.normChunks)
	dir[ci] = chunk
	s.normChunks = dir
}

// normAt returns the cached norm for a stored ordinal, or zero for metrics
// that do not use norms.
func (s *vectorSnapshot) normAt(ord int) float64 {
	if s.cfg.Metric == MetricCosine {
		return s.normChunks[ord>>vectorChunkShift][ord&(vectorChunkVectors-1)]
	}
	return 0
}

// distQuery computes the metric distance from query (with precomputed norm)
// to stored ordinal ord. Cosine reuses cached norms; other metrics call the
// pre-resolved raw kernel directly.
func (s *vectorSnapshot) distQuery(query []byte, queryNorm float64, ord int) float64 {
	if s.cfg.Metric == MetricCosine {
		return cosineFromDotNorms(s.dot(query, s.vectorAt(ord)), queryNorm, s.normAt(ord))
	}
	return s.dist(query, s.vectorAt(ord))
}

// distOrds computes the metric distance between two stored ordinals. Cosine
// reuses both cached norms.
func (s *vectorSnapshot) distOrds(a, b int) float64 {
	if s.cfg.Metric == MetricCosine {
		return cosineFromDotNorms(s.dot(s.vectorAt(a), s.vectorAt(b)), s.normAt(a), s.normAt(b))
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
func (s *vectorSnapshot) searchLayer(ctx *SearchContext, query []byte, queryNorm float64, filter CandidateFilter, entryOrd, ef, level int) []searchEntry {
	gen := ctx.gen
	visited := ctx.visited
	cand := &ctx.cand
	best := &ctx.best
	filtered := &ctx.filtered
	useFiltered := filter != nil && s.cfg.FilterAware
	if useFiltered {
		filtered.reset()
	}

	entryDist := s.distQuery(query, queryNorm, entryOrd)
	visited[entryOrd] = gen
	entry := searchEntry{ord: entryOrd, dist: entryDist}
	cand.push(entry)
	best.push(entry)
	if useFiltered && filter(s.ids[entryOrd]) {
		filtered.push(entry)
	}

	checks := 0
	for cand.len() != 0 {
		if ctx.ctx != nil {
			checks++
			if checks&255 == 0 && ctx.cancelled() {
				break
			}
		}
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
			entry := searchEntry{ord: nOrd, dist: d}
			if best.len() < ef || d < best.peek().dist {
				cand.push(entry)
				best.push(entry)
				if best.len() > ef {
					best.pop()
				}
			}
			if useFiltered && filter(s.ids[nOrd]) {
				if filtered.len() < ef || d < filtered.peek().dist {
					filtered.push(entry)
					if filtered.len() > ef {
						filtered.pop()
					}
				}
			}
		}
	}
	if ctx.ctx != nil && ctx.cancelled() {
		return nil
	}
	source := best.items
	if useFiltered && filtered.len() != 0 {
		source = filtered.items
	}
	out := append(ctx.layer[:0], source...)
	ctx.layer = out
	return out
}

// layer0Search descends from the entry to layer 0 then explores with budget
// ef, returning raw entries (unfiltered, may include tombstones).
func (s *vectorSnapshot) layer0Search(ctx *SearchContext, query []byte, filter CandidateFilter, ef int) []searchEntry {
	entryOrd, ok := s.locate(s.entry)
	if !ok {
		return nil
	}
	cur := entryOrd
	for l := s.maxLevel; l > 0; l-- {
		cur = s.greedyStep(cur, l, query, ctx.queryNorm)
	}
	return s.searchLayer(ctx, query, ctx.queryNorm, filter, cur, ef, 0)
}

// effectiveEf computes the layer-0 exploration budget for k, applying the
// dynamic-ef policy (Weaviate dynamicEf analogue) and filtered oversampling.
func (s *vectorSnapshot) effectiveEf(k int, filter CandidateFilter, filterHint int, efOverride, oversample uint32) int {
	ef := int(s.cfg.EfSearch)
	if efOverride != 0 {
		ef = int(efOverride)
	} else if s.cfg.DynamicEfFactor > 0 {
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
		factor := s.cfg.OversampleFactor
		if oversample != 0 {
			factor = oversample
		} else if filterHint > 0 && filterHint <= len(s.ids) && filterHint*2 >= len(s.ids) && factor > 6 {
			// Filters matching at least half the index already yield plenty
			// of candidates per ef step; 6x oversampling is enough. Keep the
			// configured factor for narrower filters where recall is more
			// sensitive to the traversal budget.
			factor = 6
		}
		if want := k * int(factor); want > ef {
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
// Filter-aware traversal may already have selected filter-passing entries;
// assemble still rechecks them because it also serves the exact and
// filter-disabled paths.
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
		out = append(out, Result{ValueID: widenedID(s.ids[e.ord]), Score: encodeScoreForMetric(metric, e.dist, querySelfDot), Distance: float32(e.dist)})
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
		if ctx.ctx != nil && ord&4095 == 0 && ctx.cancelled() {
			break
		}
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
	if ctx.ctx != nil && ctx.cancelled() {
		return buf[:0]
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
		out = append(out, Result{ValueID: widenedID(s.ids[e.ord]), Score: encodeScoreForMetric(metric, e.dist, querySelfDot), Distance: float32(e.dist)})
	}
	return out
}

// exactSearch is the scalar exact reference for the index metric: full scan
// over live nodes honoring tombstones and filter, sorted, truncated to k.
func (s *vectorSnapshot) exactSearch(ctx context.Context, query []byte, filter CandidateFilter, k int) ([]Result, error) {
	if k <= 0 || len(s.ids) == 0 {
		return nil, nil
	}
	type scored struct {
		id   NodeID
		dist float64
	}
	queryNorm := s.rawNorm(query)
	all := make([]scored, 0, len(s.ids))
	for ord, id := range s.ids {
		if ctx != nil && ord&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if s.tomb.Contains(uint64(id)) {
			continue
		}
		if filter != nil && !filter(id) {
			continue
		}
		all = append(all, scored{id: id, dist: s.distQuery(query, queryNorm, ord)})
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		out[i] = Result{ValueID: widenedID(sc.id), Score: encodeScoreForMetric(metric, sc.dist, querySelfDot), Distance: float32(sc.dist)}
	}
	return out, nil
}

// searchCandidatesInto scores a caller-supplied set of unique NodeIDs and
// appends the best k to dst. It is the exact, filter-first path for selective
// metadata/ACL predicates: distance work is proportional to the candidate set,
// not the index size.
func (s *vectorSnapshot) searchCandidatesInto(ctx *SearchContext, dst []Result, candidates []NodeID, query []byte, k int) []Result {
	if k <= 0 || len(candidates) == 0 {
		return dst
	}
	ctx.cand.reset()
	ctx.best.reset()
	ctx.layer = ctx.layer[:0]
	ctx.queryNorm = s.rawNorm(query)
	best := &ctx.best
	checks := 0
	for _, id := range candidates {
		if ctx.ctx != nil {
			checks++
			if checks&1023 == 0 && ctx.cancelled() {
				break
			}
		}
		ord, ok := s.locate(id)
		if !ok || s.tomb.Contains(uint64(id)) {
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
	if ctx.ctx != nil && ctx.cancelled() {
		return dst
	}
	if ctx.err != nil {
		return dst
	}
	all := append(ctx.layer[:0], best.items...)
	ctx.layer = all
	sortAssembled(all, s.ids)
	metric := s.cfg.Metric
	var querySelfDot float64
	if metric == MetricDot {
		querySelfDot = dotSelfBytes(s.cfg, query)
	}
	for _, e := range all {
		dst = append(dst, Result{
			ValueID:  widenedID(s.ids[e.ord]),
			Score:    encodeScoreForMetric(metric, e.dist, querySelfDot),
			Distance: float32(e.dist),
		})
	}
	return dst
}

func (s *vectorSnapshot) searchCandidatesFloat32(queryCtx context.Context, dst []Result, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if k <= 0 {
		return dst, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return dst, err
		}
	}
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	ctx.ctx = queryCtx
	ctx.err = nil
	if cap(ctx.queryBuf) < s.vecBytes {
		ctx.queryBuf = make([]byte, s.vecBytes)
	}
	raw := encodeRawFloat32(ctx.queryBuf[:0], query)
	ctx.queryBuf = raw
	out := s.searchCandidatesInto(ctx, dst, candidates, raw, k)
	if ctx.err != nil {
		return dst, ctx.err
	}
	return out, nil
}

// searchWithHint runs the full ANN path with an optional filter cardinality
// hint for the cost-based flat fallback. flatSmall reports whether the flat
// path was taken.
func (s *vectorSnapshot) searchWithHint(ctx *SearchContext, query []byte, filter CandidateFilter, k int, filterHint int, efOverride, oversample uint32) ([]Result, bool) {
	if k > len(s.ids) {
		k = len(s.ids)
	}
	ctx.queryNorm = s.rawNorm(query)
	live := len(s.ids) - len(s.tombSorted)
	if s.cfg.FlatThreshold >= 0 && live <= s.cfg.FlatThreshold {
		out := s.exactSearchInto(ctx, query, filter, k, ctx.output[:0])
		ctx.output = out
		if ctx.err != nil {
			return nil, true
		}
		return out, true
	}
	if filter != nil && filterHint >= 0 && s.cfg.FlatSearchCutoff >= 0 && filterHint <= s.cfg.FlatSearchCutoff {
		out := s.exactSearchInto(ctx, query, filter, k, ctx.output[:0])
		ctx.output = out
		if ctx.err != nil {
			return nil, true
		}
		return out, true
	}
	var querySelfDot float64
	if s.cfg.Metric == MetricDot {
		querySelfDot = dotSelfBytes(s.cfg, query)
	}
	ef := s.effectiveEf(k, filter, filterHint, efOverride, oversample)
	ctx.resetForNodes(len(s.ids), s.vecBytes)
	entries := s.layer0Search(ctx, query, filter, ef)
	if ctx.err != nil {
		return nil, false
	}
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
				entries = s.layer0Search(ctx, query, filter, ef2)
				if ctx.err != nil {
					return nil, false
				}
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
				exact, err := s.exactSearch(ctx.ctx, query, filter, k)
				if err != nil {
					ctx.err = err
					return nil, false
				}
				if ctx.err != nil {
					return nil, false
				}
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
