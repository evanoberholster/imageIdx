package store

import "sync/atomic"

// Stats is a cheap, cumulative view of physical Pebble operations initiated by
// this Store. It is intended for benchmarks and operator diagnostics; it is not
// a transactional snapshot across concurrent writers.
type Stats struct {
	Commits      uint64
	Sets         uint64
	Deletes      uint64
	DeleteRanges uint64
	KeyBytes     uint64
	ValueBytes   uint64
}

type storeStats struct {
	commits      atomic.Uint64
	sets         atomic.Uint64
	deletes      atomic.Uint64
	deleteRanges atomic.Uint64
	keyBytes     atomic.Uint64
	valueBytes   atomic.Uint64
}

func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		Commits:      s.stats.commits.Load(),
		Sets:         s.stats.sets.Load(),
		Deletes:      s.stats.deletes.Load(),
		DeleteRanges: s.stats.deleteRanges.Load(),
		KeyBytes:     s.stats.keyBytes.Load(),
		ValueBytes:   s.stats.valueBytes.Load(),
	}
}
