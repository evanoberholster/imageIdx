package timeidx

import (
	"cmp"
	"slices"
	"sort"

	"github.com/evanoberholster/imageIdx/common"
)

const recordBytes = 16

type shard struct {
	times          []uint64
	ids            []common.ValueID
	offset         uint64
	length         uint32
	minID, maxID   common.ValueID
	timeCRC, idCRC uint32
}

type snapshot struct {
	shards           []*shard
	generation       uint64
	sourceGeneration uint64
	count            uint64
}

type writeScratch struct {
	adds       []Mutation
	starts     []int
	buffers    []shardBuffer
	nextBuffer int
	mutations  []Mutation
}

type shardBuffer struct {
	times []uint64
	ids   []common.ValueID
}

// One backing allocation, two non-overlapping SoA columns. Limit each column's
// capacity so appending to times can never overwrite the ID plane.
func makeColumns(count int) ([]uint64, []common.ValueID) {
	data := make([]uint64, 2*count)
	return data[:0:count], data[count : count : 2*count]
}

// Persistent commits copy these private columns to mmap before publication,
// so their temporary merge buffers can be reused. Heap snapshots instead own
// fresh columns, using the same merge algorithm and never reusing reader data.
func (scratch *writeScratch) columns(count int, reuse bool) ([]uint64, []common.ValueID) {
	if !reuse {
		return makeColumns(count)
	}
	if scratch.nextBuffer == len(scratch.buffers) {
		scratch.buffers = append(scratch.buffers, shardBuffer{})
	}
	b := &scratch.buffers[scratch.nextBuffer]
	scratch.nextBuffer++
	if cap(b.times) < count {
		b.times, b.ids = makeColumns(count)
	}
	return b.times[:0], b.ids[:0]
}

func pairLess(at uint64, ai common.ValueID, bt uint64, bi common.ValueID) bool {
	return at < bt || (at == bt && ai < bi)
}

// Batch-local negative filter only: a set bit still requires exact membership.
// Mix high and low ID bits so aligned/sparse identifiers do not share one bit.
func mutationBit(id uint64) uint64 { return ((id ^ (id >> 32)) * 0x9e3779b97f4a7c15) >> 52 }

func shardFor(shards []*shard, timestamp uint64, id common.ValueID) int {
	if len(shards) == 0 {
		return -1
	}
	return max(0, sort.Search(len(shards), func(n int) bool {
		s := shards[n]
		return pairLess(timestamp, id, s.times[0], s.ids[0])
	})-1)
}

func newShard(times []uint64, ids []common.ValueID) *shard {
	s := &shard{times: times, ids: ids}
	if len(ids) != 0 {
		s.minID, s.maxID = ids[0], ids[0]
	}
	for _, id := range ids {
		s.minID = min(s.minID, id)
		s.maxID = max(s.maxID, id)
	}
	return s
}

func splitShards(dst []*shard, times []uint64, ids []common.ValueID, maxRecords int) []*shard {
	chunks := (len(times) + maxRecords - 1) / maxRecords
	for n := 0; n < chunks; n++ {
		start, end := n*len(times)/chunks, (n+1)*len(times)/chunks
		dst = append(dst, newShard(times[start:end], ids[start:end]))
	}
	return dst
}

func initialShards(entries []Entry, maxRecords int) ([]*shard, error) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.ImageID, b.ImageID) })
	for n := 1; n < len(entries); n++ {
		if entries[n].ImageID == entries[n-1].ImageID {
			return nil, ErrDuplicateImageID
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if d := cmp.Compare(a.Timestamp, b.Timestamp); d != 0 {
			return d
		}
		return cmp.Compare(a.ImageID, b.ImageID)
	})
	times, ids := makeColumns(len(entries))
	times, ids = times[:len(entries)], ids[:len(entries)]
	for n, e := range entries {
		times[n], ids[n] = e.Timestamp, e.ImageID
	}
	return splitShards(nil, times, ids, maxRecords), nil
}

// prepare replaces only shards containing touched IDs or receiving additions.
// ID range pruning avoids unrelated scans where the domain is clustered. No
// reverse map is persisted: sparse/random IDs can require scanning many shards.
func prepare(current []*shard, ops []Mutation, maxRecords int, scratch *writeScratch, reuse bool) []*shard {
	adds := collectAdditions(scratch, ops)
	if len(current) == 0 {
		times, ids := scratch.columns(len(adds), reuse)
		times = times[:len(adds)]
		ids = ids[:len(adds)]
		for n, a := range adds {
			times[n], ids[n] = a.to, a.imageID
		}
		return splitShards(nil, times, ids, maxRecords)
	}
	var starts []int
	// Deletes have no destination routing. Avoid clearing and prefix-summing
	// the entire directory merely to produce an all-zero starts array.
	if len(adds) != 0 {
		if cap(scratch.starts) < len(current)+1 {
			scratch.starts = make([]int, len(current)+1)
		}
		starts = scratch.starts[:len(current)+1]
		clear(starts)
		for _, a := range adds {
			starts[shardFor(current, a.to, a.imageID)+1]++
		}
		for n := 1; n < len(starts); n++ {
			starts[n] += starts[n-1]
		}
	}
	out := make([]*shard, 0, len(current)+1)
	minID, maxID := ops[0].imageID, ops[len(ops)-1].imageID
	var bitScratch [64]uint64
	var touchedBits []uint64
	var sparseBits [64]uint64
	if maxID-minID < uint64(len(bitScratch)*64) {
		touchedBits = bitScratch[:(maxID-minID)/64+1]
		for _, op := range ops {
			v := op.imageID - minID
			touchedBits[v>>6] |= uint64(1) << (v & 63)
		}
	} else if len(ops) > 1 {
		for _, op := range ops {
			bit := mutationBit(op.imageID)
			sparseBits[bit>>6] |= uint64(1) << (bit & 63)
		}
	}
	touched := func(id common.ValueID) bool {
		if id < minID || id > maxID {
			return false
		}
		if touchedBits != nil {
			v := id - minID
			return touchedBits[v>>6]&(uint64(1)<<(v&63)) != 0
		}
		if len(ops) > 1 {
			bit := mutationBit(id)
			if sparseBits[bit>>6]&(uint64(1)<<(bit&63)) == 0 {
				return false
			}
		}
		_, ok := slices.BinarySearchFunc(ops, id, func(a Mutation, b common.ValueID) int { return cmp.Compare(a.imageID, b) })
		return ok
	}
	for n, s := range current {
		a := adds[:0]
		if len(starts) != 0 {
			a = adds[starts[n]:starts[n+1]]
		}
		dirty := len(a) != 0
		if !dirty && s.maxID >= minID && s.minID <= maxID {
			for _, id := range s.ids {
				if touched(id) {
					dirty = true
					break
				}
			}
		}
		if !dirty {
			out = append(out, s)
			continue
		}
		times, ids := scratch.columns(len(s.times)+len(a), reuse)
		ai := 0
		// Filter each old row once, then emit all additions preceding it.
		// Advancing additions must not repeat membership checks on the same
		// retained row (or the old-row exhaustion tests) for every addition.
		for bi, ts := range s.times {
			id := s.ids[bi]
			if touched(id) {
				continue
			}
			for ai < len(a) && pairLess(a[ai].to, a[ai].imageID, ts, id) {
				times = append(times, a[ai].to)
				ids = append(ids, a[ai].imageID)
				ai++
			}
			times = append(times, ts)
			ids = append(ids, id)
		}
		for _, add := range a[ai:] {
			times = append(times, add.to)
			ids = append(ids, add.imageID)
		}
		out = splitShards(out, times, ids, maxRecords)
	}
	return coalesceAdjacentShards(out, maxRecords, scratch, reuse)
}

// collectAdditions returns the non-delete mutations in (time, ID) order, reusing
// the writer-owned scratch slice. It also resets the scratch buffer cursor so a
// fresh prepare call reuses merge columns from the start.
func collectAdditions(scratch *writeScratch, ops []Mutation) []Mutation {
	scratch.nextBuffer = 0
	if cap(scratch.adds) < len(ops) {
		scratch.adds = make([]Mutation, 0, len(ops))
	}
	adds := scratch.adds[:0]
	for _, op := range ops {
		if op.kind != mutationDelete {
			adds = append(adds, op)
		}
	}
	slices.SortFunc(adds, func(a, b Mutation) int {
		if d := cmp.Compare(a.to, b.to); d != 0 {
			return d
		}
		return cmp.Compare(a.imageID, b.imageID)
	})
	scratch.adds = adds
	return adds
}

// coalesceAdjacentShards merges adjacent NEW (never-yet-allocated) shards whose
// combined payload fits maxRecords and where at least one side is well under
// the bound. Persistent shards are never rewritten merely to improve occupancy.
func coalesceAdjacentShards(out []*shard, maxRecords int, scratch *writeScratch, reuse bool) []*shard {
	merged := out[:0]
	for _, s := range out {
		if len(merged) > 0 {
			p := merged[len(merged)-1]
			if p.offset == 0 && s.offset == 0 && len(p.times)+len(s.times) <= maxRecords &&
				(min(len(p.times), len(s.times)) < maxRecords/4) {
				times, ids := scratch.columns(len(p.times)+len(s.times), reuse)
				times = append(times, p.times...)
				times = append(times, s.times...)
				ids = append(ids, p.ids...)
				ids = append(ids, s.ids...)
				merged[len(merged)-1] = newShard(times, ids)
				continue
			}
		}
		merged = append(merged, s)
	}
	return merged
}
