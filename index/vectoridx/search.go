package vectoridx

import "context"

// queryEncoder writes the typed query into dst (reusing pooled capacity) and
// returns the raw encoding. It runs lock-free with ctx alive.
type queryEncoder func(dst []byte) []byte

// Search returns up to opts.K nearest live neighbors for a typed query.
func (v *VectorIdx) Search(query Query, opts SearchOptions) ([]Result, error) {
	return v.SearchContext(nil, query, opts)
}

// SearchContext is Search with cancellation.
func (v *VectorIdx) SearchContext(queryCtx context.Context, query Query, opts SearchOptions) ([]Result, error) {
	if _, err := query.validate(v.cfgBase); err != nil {
		return nil, err
	}
	return v.searchEncodedWithOptions(queryCtx, opts.Filter, opts.K, normalizeFilterHint(opts.FilterHint), opts.EfSearch, opts.Oversample, query.encode)
}

func (v *VectorIdx) searchEncodedWithOptions(queryCtx context.Context, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out, err := snap.searchEncodedWithOptions(queryCtx, filter, k, filterHint, ef, oversample, encode)
	v.readers.Release(token)
	return out, err
}

func (s *vectorSnapshot) searchEncodedWithOptions(queryCtx context.Context, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return nil, err
		}
	}
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	ctx.ctx = queryCtx
	ctx.err = nil
	if cap(ctx.queryBuf) < s.vecBytes {
		ctx.queryBuf = make([]byte, s.vecBytes)
	}
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	if !s.hasEntry || len(s.ids) == 0 {
		return nil, nil
	}
	if k > len(s.ids) {
		k = len(s.ids)
	}
	results, _ := s.searchWithHint(ctx, query, filter, k, filterHint, ef, oversample)
	if ctx.err != nil {
		return nil, ctx.err
	}
	out := make([]Result, len(results))
	copy(out, results)
	return out, nil
}

// SearchInto appends up to opts.K matches to dst, reusing caller storage.
func (v *VectorIdx) SearchInto(dst []Result, query Query, opts SearchOptions) ([]Result, error) {
	return v.SearchIntoContext(nil, dst, query, opts)
}

// SearchIntoContext is SearchInto with cancellation.
func (v *VectorIdx) SearchIntoContext(queryCtx context.Context, dst []Result, query Query, opts SearchOptions) ([]Result, error) {
	if _, err := query.validate(v.cfgBase); err != nil {
		return dst, err
	}
	return v.searchIntoEncodedWithOptions(queryCtx, dst, opts.Filter, opts.K, normalizeFilterHint(opts.FilterHint), opts.EfSearch, opts.Oversample, query.encode)
}

func (v *VectorIdx) searchIntoEncodedWithOptions(queryCtx context.Context, dst []Result, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return dst, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return dst, ErrClosed
	}
	dst, err := snap.searchIntoEncodedWithOptions(queryCtx, dst, filter, k, filterHint, ef, oversample, encode)
	v.readers.Release(token)
	return dst, err
}

func (s *vectorSnapshot) searchIntoEncodedWithOptions(queryCtx context.Context, dst []Result, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
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
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	if s.hasEntry && len(s.ids) != 0 {
		if k > len(s.ids) {
			k = len(s.ids)
		}
		results, _ := s.searchWithHint(ctx, query, filter, k, filterHint, ef, oversample)
		if ctx.err != nil {
			return dst, ctx.err
		}
		dst = append(dst, results...)
	}
	return dst, nil
}

// ExactSearch runs the scalar exact reference scan for a typed query.
func (v *VectorIdx) ExactSearch(query Query, filter CandidateFilter, k int) ([]Result, error) {
	return v.ExactSearchContext(nil, query, filter, k)
}

// ExactSearchContext is ExactSearch with cancellation.
func (v *VectorIdx) ExactSearchContext(queryCtx context.Context, query Query, filter CandidateFilter, k int) ([]Result, error) {
	if _, err := query.validate(v.cfgBase); err != nil {
		return nil, err
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return nil, err
		}
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out, err := snap.exactSearch(queryCtx, query.encode(nil), filter, k)
	v.readers.Release(token)
	return out, err
}

// SearchCandidates scores exactly the supplied unique candidate NodeIDs and
// returns the best k. It is the selective-filter path: cost scales with
// len(candidates), not with the index size. Tombstoned or unknown IDs are
// skipped.
func (v *VectorIdx) SearchCandidates(queryCtx context.Context, candidates []NodeID, query Query, k int) ([]Result, error) {
	if _, err := query.validate(v.cfgBase); err != nil {
		return nil, err
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	out := make([]Result, 0, min(k, len(candidates)))
	return v.SearchCandidatesInto(queryCtx, out, candidates, query, k)
}

// SearchCandidatesInto appends the best k candidate matches to dst.
func (v *VectorIdx) SearchCandidatesInto(queryCtx context.Context, dst []Result, candidates []NodeID, query Query, k int) ([]Result, error) {
	if _, err := query.validate(v.cfgBase); err != nil {
		return dst, err
	}
	if k <= 0 {
		return dst, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return dst, ErrClosed
	}
	out, err := snap.searchCandidatesEncoded(queryCtx, dst, candidates, query.encode, k)
	v.readers.Release(token)
	return out, err
}

// ---- typed conveniences ----

// SearchInt8 returns up to k nearest live neighbors of an int8 query.
func (v *VectorIdx) SearchInt8(query []int8, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Int8: query}, SearchOptions{K: k, Filter: filter})
}

// SearchUint8 returns up to k nearest live neighbors of a uint8 query.
func (v *VectorIdx) SearchUint8(query []uint8, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Uint8: query}, SearchOptions{K: k, Filter: filter})
}

// SearchInt16 returns up to k nearest live neighbors of an int16 query.
func (v *VectorIdx) SearchInt16(query []int16, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Int16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchUint16 returns up to k nearest live neighbors of a uint16 query.
func (v *VectorIdx) SearchUint16(query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Uint16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchFloat32 returns up to k nearest live neighbors of a float32 query.
func (v *VectorIdx) SearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Float32: query}, SearchOptions{K: k, Filter: filter})
}

// SearchFloat16 returns up to k nearest live neighbors of a binary16 query.
func (v *VectorIdx) SearchFloat16(query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	return v.Search(Query{Float16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoInt8 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt8(dst []Result, query []int8, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Int8: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoUint8 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoUint8(dst []Result, query []uint8, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Uint8: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoInt16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt16(dst []Result, query []int16, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Int16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoUint16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoUint16(dst []Result, query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Uint16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoFloat32 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoFloat32(dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Float32: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoFloat16 appends up to k binary16 matches to dst.
func (v *VectorIdx) SearchIntoFloat16(dst []Result, query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchInto(dst, Query{Float16: query}, SearchOptions{K: k, Filter: filter})
}

// SearchFloat32WithOptions is SearchFloat32 with cancellation and per-query
// ef/oversample/filter-hint controls.
func (v *VectorIdx) SearchFloat32WithOptions(queryCtx context.Context, query []float32, opts SearchOptions) ([]Result, error) {
	return v.SearchContext(queryCtx, Query{Float32: query}, opts)
}

// SearchFloat32Context is SearchFloat32 with cancellation.
func (v *VectorIdx) SearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchContext(queryCtx, Query{Float32: query}, SearchOptions{K: k, Filter: filter})
}

// SearchIntoFloat32WithOptions is SearchIntoFloat32 with cancellation and
// per-query controls. It reuses dst and pooled scratch after warm-up.
func (v *VectorIdx) SearchIntoFloat32WithOptions(queryCtx context.Context, dst []Result, query []float32, opts SearchOptions) ([]Result, error) {
	return v.SearchIntoContext(queryCtx, dst, Query{Float32: query}, opts)
}

// SearchIntoFloat32Context is SearchIntoFloat32 with cancellation.
func (v *VectorIdx) SearchIntoFloat32Context(queryCtx context.Context, dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchIntoContext(queryCtx, dst, Query{Float32: query}, SearchOptions{K: k, Filter: filter})
}

// SearchCandidatesFloat32 scores exactly the supplied unique candidate NodeIDs.
func (v *VectorIdx) SearchCandidatesFloat32(queryCtx context.Context, candidates []NodeID, query []float32, k int) ([]Result, error) {
	return v.SearchCandidates(queryCtx, candidates, Query{Float32: query}, k)
}

// SearchCandidatesIntoFloat32 appends the best k candidate matches to dst.
func (v *VectorIdx) SearchCandidatesIntoFloat32(queryCtx context.Context, dst []Result, candidates []NodeID, query []float32, k int) ([]Result, error) {
	return v.SearchCandidatesInto(queryCtx, dst, candidates, Query{Float32: query}, k)
}

// ExactSearchFloat32 runs the scalar exact reference scan for the index metric.
func (v *VectorIdx) ExactSearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.ExactSearch(Query{Float32: query}, filter, k)
}

// ExactSearchFloat32Context is ExactSearchFloat32 with cancellation.
func (v *VectorIdx) ExactSearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.ExactSearchContext(queryCtx, Query{Float32: query}, filter, k)
}

// ExactSearchFloat16 runs the exact reference scan for a binary16 query.
func (v *VectorIdx) ExactSearchFloat16(query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	return v.ExactSearch(Query{Float16: query}, filter, k)
}

// SearchWithFilterHint runs a float32 search with a filter cardinality hint
// for the cost-based flat fallback.
func (v *VectorIdx) SearchWithFilterHint(query []float32, filter CandidateFilter, k int, filterHint int) ([]Result, error) {
	return v.Search(Query{Float32: query}, SearchOptions{K: k, Filter: filter, FilterHint: filterHint})
}
