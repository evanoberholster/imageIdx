package bitmapidx

import (
	"encoding/binary"

	common "github.com/evanoberholster/imageIdx/common"
)

// BSI WAL records are logical value transitions, never per-bit changes, so one
// value update is one record regardless of how many bit slices it touches:
//
//	ID       u64 (0:8)   ValueID, never zero
//	OldValue u64 (8:16)  previous value (valid when HasOld)
//	NewValue u64 (16:24) next value (valid when HasNew)
//	Flags    u8  (24)    bit0 HasOld, bit1 HasNew
//	Reserved     (25:32) zero
//
// The frame header's group count is the number of records and its update count
// is also the record count; the payload length is exactly 32*records. Replay
// reconstructs each record's resulting state (HasNew -> set, else delete) and
// expands it back into slice memberships, so replay reproduces the original
// logical state without re-running the preconditions.
const (
	bsiWALRecordSize = 32
	bsiWALFlagHasOld = 1 << 0
	bsiWALFlagHasNew = 1 << 1
	bsiWALKnownFlag  = bsiWALFlagHasOld | bsiWALFlagHasNew
)

// bsiReplayOp is one decoded logical transition reduced to its resulting state.
type bsiReplayOp struct {
	id      common.ValueID
	value   uint32
	present bool
}

// encodeBSIChanges encodes logical transitions into a BSI WAL payload. It
// rejects zero IDs, values beyond the width, and batches beyond the update
// bound; values are never truncated.
func encodeBSIChanges(spec NamespaceSpec, ops []bsiOp) ([]byte, uint32, error) {
	if len(ops) == 0 {
		return nil, 0, nil
	}
	if len(ops) > MaxNamespaceBatchUpdates || len(ops)*bsiWALRecordSize > MaxNamespaceBatchBytes {
		return nil, 0, ErrBatchTooLarge
	}
	payload := make([]byte, len(ops)*bsiWALRecordSize)
	for n, op := range ops {
		if op.id == 0 {
			return nil, 0, ErrInvalidImageID
		}
		if op.hasOld() {
			if err := checkBSIValue(spec.BSIWidth, uint64(op.from)); err != nil {
				return nil, 0, err
			}
		}
		if op.hasNew() {
			if err := checkBSIValue(spec.BSIWidth, uint64(op.to)); err != nil {
				return nil, 0, err
			}
		}
		record := payload[n*bsiWALRecordSize : (n+1)*bsiWALRecordSize]
		binary.LittleEndian.PutUint64(record[0:8], op.id)
		binary.LittleEndian.PutUint64(record[8:16], uint64(op.from))
		binary.LittleEndian.PutUint64(record[16:24], uint64(op.to))
		var flags uint8
		if op.hasOld() {
			flags |= bsiWALFlagHasOld
		}
		if op.hasNew() {
			flags |= bsiWALFlagHasNew
		}
		record[24] = flags
	}
	return payload, uint32(len(ops)), nil
}

// decodeBSIChanges validates a BSI WAL payload and reduces it to replay ops. It
// checks the exact record layout, reserved bytes, presence flags, non-zero IDs,
// width bounds, and payload boundaries.
func decodeBSIChanges(spec NamespaceSpec, payload []byte, records uint32) ([]bsiReplayOp, uint32, error) {
	if records == 0 || records > MaxNamespaceBatchUpdates || uint64(len(payload)) != uint64(records)*bsiWALRecordSize {
		return nil, 0, ErrCorruptWAL
	}
	ops := make([]bsiReplayOp, 0, records)
	for n := uint32(0); n < records; n++ {
		record := payload[n*bsiWALRecordSize : (n+1)*bsiWALRecordSize]
		id := binary.LittleEndian.Uint64(record[0:8])
		oldValue := binary.LittleEndian.Uint64(record[8:16])
		newValue := binary.LittleEndian.Uint64(record[16:24])
		flags := record[24]
		if id == 0 || flags&^uint8(bsiWALKnownFlag) != 0 ||
			record[25] != 0 || record[26] != 0 || record[27] != 0 ||
			binary.LittleEndian.Uint32(record[28:32]) != 0 {
			return nil, 0, ErrCorruptWAL
		}
		if flags&bsiWALFlagHasOld != 0 {
			if err := checkBSIValue(spec.BSIWidth, oldValue); err != nil {
				return nil, 0, ErrCorruptWAL
			}
		} else if oldValue != 0 {
			return nil, 0, ErrCorruptWAL
		}
		if flags&bsiWALFlagHasNew != 0 {
			if err := checkBSIValue(spec.BSIWidth, newValue); err != nil {
				return nil, 0, ErrCorruptWAL
			}
			ops = append(ops, bsiReplayOp{id: id, present: true, value: uint32(newValue)})
		} else {
			if newValue != 0 {
				return nil, 0, ErrCorruptWAL
			}
			ops = append(ops, bsiReplayOp{id: id, present: false})
		}
	}
	return ops, records, nil
}

// resolveBSIBatch validates a batch's logical transitions against the current
// view, expands them into slice membership changes, and freezes the logical WAL
// payload. It performs every fallible step before the caller appends to the WAL.
func (i *Index) resolveBSIBatch(view *namespaceView, ops []bsiOp) ([]stagedChange, []byte, uint32, error) {
	if len(ops) == 0 {
		return nil, nil, 0, nil
	}
	spec := view.namespace
	var deltas [bsiMaxSlices]bsiSliceDelta
	for _, op := range ops {
		if op.id == 0 {
			return nil, nil, 0, ErrInvalidImageID
		}
		if op.hasOld() {
			if err := checkBSIValue(spec.BSIWidth, uint64(op.from)); err != nil {
				return nil, nil, 0, err
			}
		}
		if op.hasNew() {
			if err := checkBSIValue(spec.BSIWidth, uint64(op.to)); err != nil {
				return nil, nil, 0, err
			}
		}
		// Read the current value with per-bit probes: resolving every slice
		// here would fold (and allocate) untouched corrected postings for a
		// batch that usually touches only a few.
		current, present := view.bsiValueAt(spec.Namespace, spec.BSIWidth, op.id)
		if err := bsiExpand(&deltas, op, current, present); err != nil {
			return nil, nil, 0, err
		}
	}
	payload, updates, err := encodeBSIChanges(spec, ops)
	if err != nil {
		return nil, nil, 0, err
	}
	return bsiChanges(spec, &deltas), payload, updates, nil
}

// applyBSIReplay republishes decoded logical transitions. It runs before the
// store assigns ownership, so it takes only the index writer lock. Replay
// applies resulting state idempotently (set or delete) and expands it into
// slice memberships; the WAL already validated the original preconditions.
func (i *Index) applyBSIReplay(ops []bsiReplayOp) error {
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	current := i.view.Load()
	spec := current.namespace
	var deltas [bsiMaxSlices]bsiSliceDelta
	for _, op := range ops {
		value, present := current.bsiValueAt(spec.Namespace, spec.BSIWidth, op.id)
		var staged bsiOp
		if op.present {
			staged = bsiOp{id: op.id, to: op.value, flags: bsiFlagUpsert}
		} else {
			staged = bsiOp{id: op.id, flags: bsiFlagRemove}
		}
		if err := bsiExpand(&deltas, staged, value, present); err != nil {
			return err
		}
	}
	changes := bsiChanges(spec, &deltas)
	if len(changes) == 0 {
		return nil
	}
	active := composeNamespaceDelta(current, changes)
	if active == nil && current.active == nil {
		return nil
	}
	base, frozen, activeNext, rotate := i.planRotation(current, active)
	i.view.Store(i.nextView(current, base, frozen, activeNext))
	if rotate {
		i.scheduleCompaction(frozen)
	}
	return nil
}
