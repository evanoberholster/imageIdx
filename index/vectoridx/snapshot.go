package vectoridx

import (
	"context"
	"sync"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Snapshot is a read-only vector index handle pinned to one published view.
// Searches on it observe the same generation even while writers publish newer
// views. Call Release when the snapshot is no longer needed; do not use it
// concurrently with Release.
type Snapshot struct {
	view    *vectorSnapshot
	readers *storage.ReaderTracker
	token   uint32
	pinned  bool
	closed  bool
	once    sync.Once
}

// Snapshot returns a read-only handle pinned to the currently published view.
// A snapshot of a closed index is closed and all searches return ErrClosed.
func (v *VectorIdx) Snapshot() *Snapshot {
	if v == nil {
		return nil
	}
	token := v.readers.Acquire()
	if v.closed.Load() {
		v.readers.Release(token)
		return &Snapshot{closed: true}
	}
	return &Snapshot{view: v.view.Load(), readers: v.readers, token: token, pinned: true}
}

// Release releases the reader pin. It is idempotent.
func (s *Snapshot) Release() {
	if s == nil || !s.pinned {
		return
	}
	s.once.Do(func() {
		s.readers.Release(s.token)
		s.pinned = false
	})
}

// ReleaseSnapshot is an alias for Release, matching the other index families.
func (s *Snapshot) ReleaseSnapshot() { s.Release() }

// Config returns the pinned index configuration.
func (s *Snapshot) Config() Config {
	if s == nil || s.view == nil {
		return Config{}
	}
	return s.view.cfg
}

// LiveCount returns the number of live nodes in the pinned view.
func (s *Snapshot) LiveCount() int {
	if s == nil || s.view == nil {
		return 0
	}
	return len(s.view.ids) - len(s.view.tombSorted)
}

// TombstoneCount returns the number of tombstoned nodes in the pinned view.
func (s *Snapshot) TombstoneCount() int {
	if s == nil || s.view == nil {
		return 0
	}
	return len(s.view.tombSorted)
}

// Generation returns the pinned generation.
func (s *Snapshot) Generation() uint64 {
	if s == nil || s.view == nil {
		return 0
	}
	return s.view.gen
}

func (s *Snapshot) ready() error {
	if s == nil || s.closed || s.view == nil {
		return ErrClosed
	}
	return nil
}

// Err reports whether this snapshot is usable.
func (s *Snapshot) Err() error { return s.ready() }

// SearchFloat32 returns up to k nearest live neighbors in this snapshot.
func (s *Snapshot) SearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return s.SearchFloat32WithOptions(nil, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// SearchFloat32WithOptions is SearchFloat32 with cancellation and per-query
// ef/oversample/filter-hint controls.
func (s *Snapshot) SearchFloat32WithOptions(queryCtx context.Context, query []float32, opts SearchOptions) ([]Result, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.view.cfg.ElementType != ElementFloat32 || len(query) != s.view.dims {
		return nil, ErrInvalidVector
	}
	hint := opts.FilterHint
	if hint == 0 {
		hint = UnknownFilterHint
	}
	return s.view.searchEncodedWithOptions(queryCtx, opts.Filter, opts.K, hint, opts.EfSearch, opts.Oversample, func(dst []byte) []byte {
		return encodeRawFloat32(dst, query)
	})
}

// SearchFloat32Context is SearchFloat32 with cancellation.
func (s *Snapshot) SearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return s.SearchFloat32WithOptions(queryCtx, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// SearchIntoFloat32 appends matches to dst, reusing caller storage.
func (s *Snapshot) SearchIntoFloat32(dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return s.SearchIntoFloat32WithOptions(nil, dst, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// SearchIntoFloat32WithOptions appends matches to dst with per-query controls.
func (s *Snapshot) SearchIntoFloat32WithOptions(queryCtx context.Context, dst []Result, query []float32, opts SearchOptions) ([]Result, error) {
	if err := s.ready(); err != nil {
		return dst, err
	}
	if s.view.cfg.ElementType != ElementFloat32 || len(query) != s.view.dims {
		return dst, ErrInvalidVector
	}
	hint := opts.FilterHint
	if hint == 0 {
		hint = UnknownFilterHint
	}
	return s.view.searchIntoEncodedWithOptions(queryCtx, dst, opts.Filter, opts.K, hint, opts.EfSearch, opts.Oversample, func(scratch []byte) []byte {
		return encodeRawFloat32(scratch, query)
	})
}

// SearchIntoFloat32Context is SearchIntoFloat32 with cancellation.
func (s *Snapshot) SearchIntoFloat32Context(queryCtx context.Context, dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return s.SearchIntoFloat32WithOptions(queryCtx, dst, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// ExactSearchFloat32 runs the scalar exact reference scan in this snapshot.
func (s *Snapshot) ExactSearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return s.ExactSearchFloat32Context(nil, query, filter, k)
}

// ExactSearchFloat32Context is ExactSearchFloat32 with cancellation.
func (s *Snapshot) ExactSearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.view.cfg.ElementType != ElementFloat32 || len(query) != s.view.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	raw := encodeRawFloat32(nil, query)
	return s.view.exactSearch(queryCtx, raw, filter, k)
}

// SearchCandidatesFloat32 scores exactly the supplied unique candidate NodeIDs.
func (s *Snapshot) SearchCandidatesFloat32(queryCtx context.Context, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if s.view.cfg.ElementType != ElementFloat32 || len(query) != s.view.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	out := make([]Result, 0, min(k, len(candidates)))
	return s.SearchCandidatesIntoFloat32(queryCtx, out, candidates, query, k)
}

// SearchCandidatesIntoFloat32 appends the best candidate matches to dst.
func (s *Snapshot) SearchCandidatesIntoFloat32(queryCtx context.Context, dst []Result, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if err := s.ready(); err != nil {
		return dst, err
	}
	if s.view.cfg.ElementType != ElementFloat32 || len(query) != s.view.dims {
		return dst, ErrInvalidVector
	}
	return s.view.searchCandidatesFloat32(queryCtx, dst, candidates, query, k)
}
