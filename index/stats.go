package index

import "sync/atomic"

// Stats is a cumulative, best-effort view of unified-index writer activity.
// Engine-internal syncs are not all observable here; the counters intentionally
// cover the unified apply, manifest and completeness paths without adding a
// metrics map to hot paths.
type Stats struct {
	Applies        uint64
	ManifestWrites uint64
	Syncs          uint64
	Checkpoints    uint64
}

type indexStats struct {
	applies        atomic.Uint64
	manifestWrites atomic.Uint64
	syncs          atomic.Uint64
	checkpoints    atomic.Uint64
}

func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		Applies:        s.stats.applies.Load(),
		ManifestWrites: s.stats.manifestWrites.Load(),
		Syncs:          s.stats.syncs.Load(),
		Checkpoints:    s.stats.checkpoints.Load(),
	}
}
