package vectoridx

import (
	"errors"
	"sort"
)

// ---- typed Add wrappers ----

// addEncoded validates a typed vector and publishes it under the writer lock.
func (v *VectorIdx) addEncoded(id NodeID, elem ElementType, n int, encode queryEncoder) error {
	if v.cfgBase.ElementType != elem || n != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encode(v.encodeBuf[:0])
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddInt8 inserts one int8 vector. A live duplicate NodeID returns
// ErrDuplicateNodeID; re-adding a tombstoned NodeID replaces its vector and
// reconnects it, so Delete followed by Add is an update.
func (v *VectorIdx) AddInt8(id NodeID, vec []int8) error {
	return v.addEncoded(id, ElementInt8, len(vec), func(dst []byte) []byte {
		return encodeRawInt8(dst, vec)
	})
}

// AddUint8 inserts one uint8 vector.
func (v *VectorIdx) AddUint8(id NodeID, vec []uint8) error {
	return v.addEncoded(id, ElementUint8, len(vec), func(dst []byte) []byte {
		return encodeRawUint8(dst, vec)
	})
}

// AddInt16 inserts one int16 vector.
func (v *VectorIdx) AddInt16(id NodeID, vec []int16) error {
	return v.addEncoded(id, ElementInt16, len(vec), func(dst []byte) []byte {
		return encodeRawInt16(dst, vec)
	})
}

// AddUint16 inserts one uint16 vector.
func (v *VectorIdx) AddUint16(id NodeID, vec []uint16) error {
	return v.addEncoded(id, ElementUint16, len(vec), func(dst []byte) []byte {
		return encodeRawUint16(dst, vec)
	})
}

// AddFloat32 inserts one float32 vector.
func (v *VectorIdx) AddFloat32(id NodeID, vec []float32) error {
	return v.addEncoded(id, ElementFloat32, len(vec), func(dst []byte) []byte {
		return encodeRawFloat32(dst, vec)
	})
}

// AddFloat32Batch inserts vectors as one writer batch. It validates duplicate
// IDs and vector widths before writing, holds writeMu once, publishes one
// snapshot, and performs one durability sync. Graph construction remains
// single-writer so replay and neighbor order stay deterministic.
func (v *VectorIdx) AddFloat32Batch(ids []NodeID, vecs [][]float32) error {
	if v.cfgBase.ElementType != ElementFloat32 || len(ids) != len(vecs) {
		return ErrInvalidVector
	}
	if len(ids) == 0 {
		return nil
	}
	for _, vec := range vecs {
		if len(vec) != v.dims {
			return ErrInvalidVector
		}
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	cur := v.view.Load()
	seen := make(map[NodeID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			v.writeMu.Unlock()
			return ErrDuplicateNodeID
		}
		seen[id] = struct{}{}
		if ord, ok := cur.locate(id); ok && !cur.tomb.Contains(uint64(id)) {
			_ = ord
			v.writeMu.Unlock()
			return ErrDuplicateNodeID
		}
	}
	for i, id := range ids {
		raw := encodeRawFloat32(v.encodeBuf[:0], vecs[i])
		v.encodeBuf = raw
		ns, err := v.addRawLocked(cur, id, raw)
		if err != nil {
			v.view.Store(cur)
			v.maybeAutoCompactLocked(cur)
			wal := v.takePendingSyncLocked()
			v.writeMu.Unlock()
			return errors.Join(err, v.syncWALAfterWrite(wal))
		}
		cur = ns
	}
	v.view.Store(cur)
	v.maybeAutoCompactLocked(cur)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// DeleteBatch tombstones ids as one writer batch with one publish/sync.
// Unknown IDs fail the batch before any WAL write; already-tombstoned IDs are
// ignored.
func (v *VectorIdx) DeleteBatch(ids []NodeID) error {
	if len(ids) == 0 {
		return nil
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	cur := v.view.Load()
	for _, id := range ids {
		if _, ok := cur.locate(id); !ok {
			v.writeMu.Unlock()
			return ErrNodeNotFound
		}
	}
	ns := cloneSnapshot(cur)
	deleted := make([]NodeID, 0, len(ids))
	for _, id := range ids {
		if cur.tomb.Contains(uint64(id)) {
			continue
		}
		if v.wal != nil {
			if err := v.appendWALRecord(walTypeDelete, id, nil); err != nil {
				v.view.Store(cur)
				wal := v.takePendingSyncLocked()
				v.writeMu.Unlock()
				return errors.Join(err, v.syncWALAfterWrite(wal))
			}
			v.lsn++
		}
		deleted = append(deleted, id)
		v.markPendingLocked(id)
	}
	tomb := make([]NodeID, 0, len(ns.tombSorted)+len(deleted))
	tomb = append(tomb, ns.tombSorted...)
	tomb = append(tomb, deleted...)
	sort.Slice(tomb, func(i, j int) bool { return tomb[i] < tomb[j] })
	if len(tomb) > 1 {
		out := tomb[:1]
		for _, id := range tomb[1:] {
			if id != out[len(out)-1] {
				out = append(out, id)
			}
		}
		tomb = out
	}
	ns.tombSorted = tomb
	ns.tomb, ns.tombSorted = rebuildTombeset(tomb)
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddFloat16 inserts one binary16 vector.
func (v *VectorIdx) AddFloat16(id NodeID, vec []uint16) error {
	return v.addEncoded(id, ElementFloat16, len(vec), func(dst []byte) []byte {
		return encodeRawFloat16(dst, vec)
	})
}
