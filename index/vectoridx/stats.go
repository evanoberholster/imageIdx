package vectoridx

import (
	"os"
	"time"
)

// CompactionResult summarizes one Compact: node counts before and after the
// rebuild, tombstones purged, and the approximate vector payload bytes
// reclaimed. Duration covers the graph rebuild and the checkpoint.
type CompactionResult struct {
	NodesBefore      int
	NodesAfter       int
	TombstonesPurged int
	BytesReclaimed   int64
	Duration         time.Duration
}

// Stats is a point-in-time snapshot of index size and maintenance counters.
// Counts come from the published snapshot and atomics; file sizes are read
// from disk on demand, so Stats never blocks on a running write.
type Stats struct {
	Generation      uint64
	LiveCount       int
	TombstoneCount  int
	FileBytes       uint64 // checkpoint file on disk
	WALBytes        uint64 // WAL segments on disk
	TotalBytes      uint64
	WALSegments     int
	WALRecords      uint64 // records appended since open or Create
	WALBytesWritten uint64 // framed bytes appended since open or Create
	WALSyncs        uint64 // explicit fsync calls
	CheckpointLSN   uint64
	PendingOps      int64
	SyncInterval    time.Duration
	LastCompaction  CompactionResult
}

// Stats returns a snapshot of size and maintenance counters. In-memory
// indexes report zero file sizes.
func (v *VectorIdx) Stats() Stats {
	snap := v.view.Load()
	s := Stats{
		Generation:      v.gen.Load(),
		LiveCount:       len(snap.ids) - len(snap.tombSorted),
		TombstoneCount:  len(snap.tombSorted),
		WALRecords:      v.walRecords.Load(),
		WALBytesWritten: v.walBytes.Load(),
		WALSyncs:        v.walSyncs.Load(),
		CheckpointLSN:   v.checkpointLSNPub.Load(),
		PendingOps:      v.asyncPending.Load(),
		SyncInterval:    v.syncInterval,
	}
	if cmp := v.lastCompact.Load(); cmp != nil {
		s.LastCompaction = *cmp
	}
	if v.path != "" {
		if info, err := os.Stat(v.path); err == nil && info.Size() > 0 {
			s.FileBytes = uint64(info.Size())
		}
		if v.walPath != "" {
			if info, err := os.Stat(v.walPath); err == nil && info.Size() > 0 {
				s.WALBytes = uint64(info.Size())
				s.WALSegments = 1
			}
		}
	}
	s.TotalBytes = s.FileBytes + s.WALBytes
	return s
}

// FileSize returns the total bytes used by the index (checkpoint plus WAL).
func (v *VectorIdx) FileSize() (int64, error) {
	if v.path == "" {
		return 0, nil
	}
	info, err := os.Stat(v.path)
	if err != nil {
		return 0, err
	}
	total := info.Size()
	if v.walPath != "" {
		if walInfo, err := os.Stat(v.walPath); err == nil {
			total += walInfo.Size()
		} else if !os.IsNotExist(err) {
			return 0, err
		}
	}
	return total, nil
}
