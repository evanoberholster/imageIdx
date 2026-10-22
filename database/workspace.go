package database

import (
	"github.com/evanoberholster/imageIdx/index"
	schema "github.com/evanoberholster/imageIdx/schema"
)

const (
	projectionBlockSize            = 64 << 10
	defaultRetainedOrdinaryBlocks  = 16
	defaultMaxProjectionBytes      = 1 << 20
	defaultOversizedRetentionBytes = 4 << 20
)

// dataRef identifies one contiguous payload inside a coordinator-owned block.
// The epoch makes stale references detectable during tests and debugging; a
// reference is only valid for the workspace epoch in which it was created.
type dataRef struct {
	slot   uint32
	offset uint32
	length uint32
	epoch  uint32
	valid  bool
}

type projectionBlock struct {
	bytes    []byte
	used     uint32
	ordinary bool
}

type queuedMutation struct {
	sequence uint64
	entity   schema.EntityKind
	id       schema.ValueID
	before   dataRef
	after    dataRef
	beforeN  uint16
	afterN   uint16
	deleted  bool
	insert   bool
	reinsert bool
}

type decodedEvent struct {
	before index.Entry
	after  index.Entry
}

type mutationGroup struct {
	entity    schema.EntityKind
	id        schema.ValueID
	first     int
	last      int
	sawDelete bool
}

// coordinatorWorkspace owns every reusable allocation used by the single
// journal publisher. It is not shared: reads never see these buffers, and the
// coordinator resets it only after the index Apply call has returned.
type coordinatorWorkspace struct {
	blocks []*projectionBlock

	mutations [64]queuedMutation
	count     int

	logicalBytes int
	epoch        uint32

	decoded [64]decodedEvent
	diffs   []projectionScratch
	scratch [64]DecodeScratch
	groups  [64]mutationGroup
	groupN  int

	maxProjectionBytes int
	oversizedRetention int
}

func newCoordinatorWorkspace(maxProjectionBytes, oversizedRetention int) *coordinatorWorkspace {
	if maxProjectionBytes <= 0 {
		maxProjectionBytes = defaultMaxProjectionBytes
	}
	if oversizedRetention <= 0 {
		oversizedRetention = defaultOversizedRetentionBytes
	}
	return &coordinatorWorkspace{
		maxProjectionBytes: maxProjectionBytes,
		oversizedRetention: oversizedRetention,
	}
}

func (w *coordinatorWorkspace) reset() {
	w.epoch++
	w.count = 0
	w.groupN = 0
	w.logicalBytes = 0

	for i := range w.blocks {
		w.blocks[i].used = 0
	}
	for i := range w.decoded {
		resetDecodedEvent(&w.decoded[i])
	}
	for i := range w.diffs {
		w.diffs[i].reset()
	}
	for i := range w.scratch {
		w.scratch[i].Bytes = w.scratch[i].Bytes[:0]
	}
	w.trimBlocks()
}

// trimBlocks keeps the ordinary high-water mark bounded and at most one
// reusable oversized buffer. Spikes above the ceiling are released.
func (w *coordinatorWorkspace) trimBlocks() {
	ordinary := 0
	keptOversized := false
	out := w.blocks[:0]
	for _, block := range w.blocks {
		if block.ordinary {
			if ordinary < defaultRetainedOrdinaryBlocks {
				out = append(out, block)
				ordinary++
			}
			continue
		}
		if !keptOversized && cap(block.bytes) <= w.oversizedRetention {
			out = append(out, block)
			keptOversized = true
		}
	}
	for i := len(out); i < len(w.blocks); i++ {
		w.blocks[i] = nil
	}
	w.blocks = out
}

func (w *coordinatorWorkspace) appendMutation(sequence uint64, raw []byte, entry journalEntry) bool {
	beforeLen, afterLen := len(entry.before), len(entry.after)
	logical := beforeLen + afterLen
	if w.count > 0 && w.logicalBytes+logical > w.maxProjectionBytes {
		return false
	}
	if w.count >= len(w.mutations) {
		return false
	}
	payload := raw[journalHeader : len(raw)-4]
	if len(payload) != logical {
		return false
	}
	slot, offset, ok := w.copyPayload(payload)
	if !ok {
		return false
	}
	m := queuedMutation{
		sequence: sequence,
		entity:   entry.entity,
		id:       entry.id,
		beforeN:  entry.beforeN,
		afterN:   entry.afterN,
		deleted:  entry.deleted,
		insert:   entry.insert,
		reinsert: entry.reinsert,
	}
	if beforeLen != 0 {
		m.before = dataRef{slot: slot, offset: offset, length: uint32(beforeLen), epoch: w.epoch, valid: true}
	}
	if afterLen != 0 {
		m.after = dataRef{slot: slot, offset: offset + uint32(beforeLen), length: uint32(afterLen), epoch: w.epoch, valid: true}
	}
	w.mutations[w.count] = m
	w.count++
	w.logicalBytes += logical
	return true
}

func (w *coordinatorWorkspace) copyPayload(payload []byte) (slot, offset uint32, ok bool) {
	if len(payload) == 0 {
		return 0, 0, true
	}
	if len(payload) <= projectionBlockSize {
		for i, block := range w.blocks {
			if !block.ordinary || int(block.used)+len(payload) > len(block.bytes) {
				continue
			}
			offset = block.used
			copy(block.bytes[offset:], payload)
			block.used += uint32(len(payload))
			return uint32(i), offset, true
		}
		block := &projectionBlock{bytes: make([]byte, projectionBlockSize), ordinary: true}
		w.blocks = append(w.blocks, block)
		copy(block.bytes, payload)
		block.used = uint32(len(payload))
		return uint32(len(w.blocks) - 1), 0, true
	}

	for i, block := range w.blocks {
		if block.ordinary || block.used != 0 || cap(block.bytes) < len(payload) || cap(block.bytes) > w.oversizedRetention {
			continue
		}
		block.bytes = block.bytes[:cap(block.bytes)]
		copy(block.bytes, payload)
		block.used = uint32(len(payload))
		return uint32(i), 0, true
	}
	block := &projectionBlock{bytes: make([]byte, len(payload))}
	w.blocks = append(w.blocks, block)
	copy(block.bytes, payload)
	block.used = uint32(len(payload))
	return uint32(len(w.blocks) - 1), 0, true
}

func (w *coordinatorWorkspace) refBytes(ref dataRef) []byte {
	if !ref.valid || ref.length == 0 {
		return nil
	}
	if ref.epoch != w.epoch {
		panic("database: stale projection reference")
	}
	if int(ref.slot) >= len(w.blocks) {
		panic("database: projection reference slot out of range")
	}
	block := w.blocks[ref.slot]
	end := uint64(ref.offset) + uint64(ref.length)
	if end > uint64(len(block.bytes)) {
		panic("database: projection reference out of range")
	}
	return block.bytes[ref.offset:end]
}

func (w *coordinatorWorkspace) hasRepeatedKey() bool {
	for i := 0; i < w.count; i++ {
		for j := i + 1; j < w.count; j++ {
			if w.mutations[i].entity == w.mutations[j].entity && w.mutations[i].id == w.mutations[j].id {
				return true
			}
		}
	}
	return false
}

// diff returns the per-mutation scratch slot, allocating the bounded directory
// only when an update actually needs projection diffing. Add/delete-only batches
// do not pay for the larger scratch structure.
func (w *coordinatorWorkspace) diff(i int) *projectionScratch {
	if i >= len(w.diffs) {
		if i >= cap(w.diffs) {
			next := make([]projectionScratch, len(w.mutations))
			copy(next, w.diffs)
			w.diffs = next
		}
		w.diffs = w.diffs[:i+1]
	}
	return &w.diffs[i]
}

func resetDecodedEvent(event *decodedEvent) {
	resetEntry(&event.before)
	resetEntry(&event.after)
}

func resetEntry(entry *index.Entry) {
	clear(entry.Times)
	entry.Times = entry.Times[:0]
	clear(entry.Geos)
	entry.Geos = entry.Geos[:0]
	clear(entry.Sims)
	entry.Sims = entry.Sims[:0]
	clear(entry.Hashes)
	entry.Hashes = entry.Hashes[:0]
	clear(entry.Vectors)
	entry.Vectors = entry.Vectors[:0]
	clear(entry.Bitmaps)
	entry.Bitmaps = entry.Bitmaps[:0]
	entry.ValueID = 0
}
