package database

import (
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/store"
)

// Stats is a cumulative, best-effort snapshot of coordinator and physical
// writer activity. It is not a transactional snapshot across concurrent
// writers; use it for benchmarks and diagnostics, not durability decisions.
type Stats struct {
	CanonicalCommits        uint64
	JournalEntries          uint64
	JournalBytes            uint64
	GenerationIntentCommits uint64
	IndexApplies            uint64
	AcknowledgmentCommits   uint64
	CleanupCommits          uint64
	Store                   store.Stats
	Index                   index.Stats
}

type dbStats struct {
	canonicalCommits        atomic.Uint64
	journalEntries          atomic.Uint64
	journalBytes            atomic.Uint64
	generationIntentCommits atomic.Uint64
	indexApplies            atomic.Uint64
	acknowledgmentCommits   atomic.Uint64
	cleanupCommits          atomic.Uint64
}

func (db *DB) Stats() Stats {
	if db == nil {
		return Stats{}
	}
	stats := Stats{
		CanonicalCommits:        db.stats.canonicalCommits.Load(),
		JournalEntries:          db.stats.journalEntries.Load(),
		JournalBytes:            db.stats.journalBytes.Load(),
		GenerationIntentCommits: db.stats.generationIntentCommits.Load(),
		IndexApplies:            db.stats.indexApplies.Load(),
		AcknowledgmentCommits:   db.stats.acknowledgmentCommits.Load(),
		CleanupCommits:          db.stats.cleanupCommits.Load(),
	}
	if db.values != nil {
		stats.Store = db.values.Stats()
	}
	if db.indexes != nil {
		stats.Index = db.indexes.Stats()
	}
	return stats
}
