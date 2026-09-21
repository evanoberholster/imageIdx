package vectoridx

import (
	"os"
	"time"
)

// CompactionResult summarizes one Compact: node counts before and after the
// rebuild, tombstones purged, delta generations merged into the base,
// records reclaimed by promotion (superseded puts and tombstones), and the
// approximate vector payload bytes reclaimed. Duration covers the graph
// rebuild and the base write.
type CompactionResult struct {
	NodesBefore      int
	NodesAfter       int
	TombstonesPurged int
	DeltasMerged     int
	ReclaimedRecords int64
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
	FileBytes       uint64 // base generation file on disk
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

	// Delta generations persisted since the base write and their promotion
	// counters.
	Deltas             int
	DeltaBytes         uint64
	DeltaRecords       uint64
	DeltaTombstones    uint64
	DeltaFragmentation float64
	CoalescedRecords   uint64
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
	if m := v.manifest.Load(); m != nil {
		s.Deltas = len(m.deltas)
		for _, d := range m.deltas {
			s.DeltaBytes += d.bytes
			s.DeltaRecords += d.records
			s.DeltaTombstones += d.tombstones
		}
		s.DeltaFragmentation = m.fragmentation()
	}
	s.CoalescedRecords = v.coalescedRecords.Load()
	if v.path != "" {
		if info, err := os.Stat(v.path); err == nil && info.Size() > 0 {
			s.FileBytes = uint64(info.Size())
		}
		segs, err := listWALSegments(v.path)
		if err == nil {
			s.WALSegments = len(segs)
			for _, seg := range segs {
				s.WALBytes += uint64(seg.size)
			}
		}
	}
	s.TotalBytes = s.FileBytes + s.WALBytes
	return s
}

// FileSize returns the total bytes used by the index (checkpoint plus WAL
// segments).
func (v *VectorIdx) FileSize() (int64, error) {
	if v.path == "" {
		return 0, nil
	}
	info, err := os.Stat(v.path)
	if err != nil {
		return 0, err
	}
	total := info.Size()
	segs, err := listWALSegments(v.path)
	if err != nil {
		return 0, err
	}
	for _, seg := range segs {
		total += seg.size
	}
	return total, nil
}
