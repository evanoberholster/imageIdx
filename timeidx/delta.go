package timeidx

import "sync/atomic"

const deltaHashMultiplier uint64 = 0x9e3779b97f4a7c15

type delta struct {
	addMarkers  []timemarker
	addImageIDs []ImageID
	deleteIDs   []ImageID

	addSequences    []uint32
	deleteSequences []uint32
	addCount        atomic.Uint32
	deleteCount     atomic.Uint32
	version         atomic.Uint64

	stateKeys []atomic.Uint64
	stateMeta []atomic.Uint64
	stateUsed []atomic.Uint32
	stateMask uint64

	capacity uint32
	opCount  uint32 // single-writer owned
}

func newDelta(capacity int) *delta {
	tableSize := 4
	for tableSize < capacity*2 {
		tableSize <<= 1
	}
	return &delta{
		addMarkers:      make([]timemarker, capacity),
		addImageIDs:     make([]ImageID, capacity),
		deleteIDs:       make([]ImageID, capacity),
		addSequences:    make([]uint32, capacity),
		deleteSequences: make([]uint32, capacity),
		stateKeys:       make([]atomic.Uint64, tableSize),
		stateMeta:       make([]atomic.Uint64, tableSize),
		stateUsed:       make([]atomic.Uint32, tableSize),
		stateMask:       uint64(tableSize - 1),
		capacity:        uint32(capacity),
	}
}

func (d *delta) len() int { return int(d.addCount.Load() + d.deleteCount.Load()) }

func (d *delta) remaining() uint32 { return d.capacity - d.opCount }

func encodeState(sequence uint32, addIndex uint32, add bool) uint64 {
	meta := uint64(sequence)<<33 | uint64(addIndex+1)<<1
	if add {
		meta |= 1
	}
	return meta
}

func decodeState(meta uint64) (sequence, addIndex uint32, add bool) {
	return uint32(meta >> 33), uint32((meta >> 1) - 1), meta&1 != 0
}

func (d *delta) stateSlot(id ImageID) (uint64, bool) {
	start := (uint64(id) * deltaHashMultiplier) & d.stateMask
	for probe := uint64(0); probe <= d.stateMask; probe++ {
		slot := (start + probe) & d.stateMask
		if d.stateUsed[slot].Load() == 0 {
			return slot, false
		}
		if ImageID(d.stateKeys[slot].Load()) == id {
			return slot, true
		}
	}
	return 0, false
}

func (d *delta) publishState(id ImageID, meta uint64) {
	slot, found := d.stateSlot(id)
	if found {
		d.stateMeta[slot].Store(meta)
		return
	}
	d.stateKeys[slot].Store(uint64(id))
	d.stateMeta[slot].Store(meta)
	d.stateUsed[slot].Store(1)
}

func (d *delta) state(id ImageID) (uint64, bool) {
	slot, found := d.stateSlot(id)
	if !found {
		return 0, false
	}
	meta := d.stateMeta[slot].Load()
	return meta, meta != 0
}

func (d *delta) appendAdd(marker timemarker, id ImageID) bool {
	if d.opCount >= d.capacity {
		return false
	}
	d.version.Add(1)
	index := d.addCount.Load()
	sequence := d.opCount + 1
	d.addMarkers[index] = marker
	d.addImageIDs[index] = id
	d.addSequences[index] = sequence
	d.publishState(id, encodeState(sequence, index, true))
	d.addCount.Store(index + 1)
	d.opCount++
	d.version.Add(1)
	return true
}

func (d *delta) appendDelete(id ImageID) bool {
	if d.opCount >= d.capacity {
		return false
	}
	d.version.Add(1)
	index := d.deleteCount.Load()
	sequence := d.opCount + 1
	d.deleteIDs[index] = id
	d.deleteSequences[index] = sequence
	d.publishState(id, encodeState(sequence, 0, false))
	d.deleteCount.Store(index + 1)
	d.opCount++
	d.version.Add(1)
	return true
}

func (d *delta) latestAdd(id ImageID, addIndex uint32) bool {
	meta, ok := d.state(id)
	if !ok {
		return false
	}
	_, latestIndex, add := decodeState(meta)
	return add && latestIndex == addIndex
}

func (d *delta) hasState(id ImageID) bool {
	_, ok := d.state(id)
	return ok
}

func (d *delta) stableVersion() uint64 {
	for {
		version := d.version.Load()
		if version&1 == 0 {
			return version
		}
	}
}

func (d *delta) reset() {
	d.version.Add(1)
	for i := range d.stateUsed {
		d.stateMeta[i].Store(0)
		d.stateUsed[i].Store(0)
	}
	d.addCount.Store(0)
	d.deleteCount.Store(0)
	d.opCount = 0
	d.version.Add(1)
}
