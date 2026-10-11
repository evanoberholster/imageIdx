package timeidx

import (
	"cmp"
	"slices"
	"sort"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/common"
)

type mutationKind uint8

const (
	mutationInsert mutationKind = iota + 1
	mutationDelete
	mutationMove
)

type Mutation struct {
	imageID  common.ValueID
	from, to uint64
	kind     mutationKind
}

// Insert is an ID-authoritative upsert. One ID has at most one time per namespace.
func Insert(id common.ValueID, timestamp uint64) Mutation {
	return Mutation{imageID: id, to: timestamp, kind: mutationInsert}
}

// Delete removes by ID; timestamp is advisory for compatibility with old callers.
func Delete(id common.ValueID, timestamp uint64) Mutation {
	return Mutation{imageID: id, from: timestamp, kind: mutationDelete}
}

// Move atomically replaces an ID's time; from is advisory, even if stale.
func Move(id common.ValueID, from, to uint64) Mutation {
	return Mutation{imageID: id, from: from, to: to, kind: mutationMove}
}

const maxBatchCapacity = 1 << 20

type Batch struct {
	ops      []Mutation
	capacity int
	inFlight atomic.Bool
}

func NewBatch(capacity int) *Batch {
	capacity = max(0, min(capacity, maxBatchCapacity))
	return &Batch{ops: make([]Mutation, 0, capacity), capacity: capacity}
}

func (b *Batch) Append(m Mutation) bool {
	if b == nil || b.inFlight.Load() || len(b.ops) == b.capacity || m.kind < mutationInsert || m.kind > mutationMove {
		return false
	}
	b.ops = append(b.ops, m)
	return true
}
func (b *Batch) Reset() bool {
	if b == nil || b.inFlight.Load() {
		return false
	}
	b.ops = b.ops[:0]
	return true
}
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return len(b.ops)
}

// Apply validates and commits one atomic batch. Input order defines last-write-
// wins for repeated IDs, including insert/delete/move combinations. Persistent
// calls synchronize data and root before returning, in BOTH durability modes.
// A failed uncertain root publication latches writes until close/reopen.
func (i *Index) Apply(b *Batch) error {
	return i.apply(b, 0, false)
}

// ApplyWithGeneration atomically commits mutations and a contiguous source
// completeness watermark. The application must supply ALL applicable changes
// through this generation; it must coordinate concurrent/out-of-order DB batches.
// Serialization cannot infer missing DB events. Decreases are rejected; equal
// generations support retries. Empty/no-op batches still persist advancement.
func (i *Index) ApplyWithGeneration(b *Batch, sourceGeneration uint64) error {
	return i.apply(b, sourceGeneration, true)
}

func (i *Index) apply(b *Batch, sourceGeneration uint64, explicit bool) error {
	if b == nil || b.ops == nil {
		return ErrUninitializedBatch
	}
	if !b.inFlight.CompareAndSwap(false, true) {
		return ErrBatchInFlight
	}
	defer b.inFlight.Store(false)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isClosed() {
		return ErrClosed
	}
	if i.failed != nil {
		return i.failed
	}
	old := i.view.Load()
	if !explicit {
		sourceGeneration = old.sourceGeneration
	}
	if sourceGeneration < old.sourceGeneration {
		return ErrSourceGeneration
	}
	advance := sourceGeneration > old.sourceGeneration
	if len(b.ops) == 0 {
		if !advance {
			return nil
		}
		return i.advanceGeneration(old, sourceGeneration)
	}
	var stack [64]Mutation
	ops := stack[:0]
	if len(b.ops) > len(stack) {
		if cap(i.scratch.mutations) < len(b.ops) {
			i.scratch.mutations = make([]Mutation, 0, len(b.ops))
		}
		ops = i.scratch.mutations[:0]
	}
	ops = append(ops, b.ops...)
	for _, m := range ops {
		if m.kind < mutationInsert || m.kind > mutationMove {
			return ErrInvalidMutation
		}
	}
	slices.SortStableFunc(ops, func(a, b Mutation) int { return cmp.Compare(a.imageID, b.imageID) })
	unique := ops[:0]
	for _, m := range ops {
		if len(unique) > 0 && unique[len(unique)-1].imageID == m.imageID {
			unique[len(unique)-1] = m
		} else {
			unique = append(unique, m)
		}
	}
	// Idempotent upserts need no new snapshot, allocations, or durability work.
	// Composite routing makes this lookup local even when a time spans shards.
	changed := unique[:0]
	for _, m := range unique {
		if m.kind != mutationDelete {
			n := shardFor(old.shards, m.to, m.imageID)
			if n >= 0 {
				s := old.shards[n]
				pos := sort.Search(len(s.times), func(k int) bool { return !pairLess(s.times[k], s.ids[k], m.to, m.imageID) })
				if pos < len(s.times) && s.times[pos] == m.to && s.ids[pos] == m.imageID {
					continue
				}
			}
		}
		changed = append(changed, m)
	}
	if len(changed) == 0 {
		if !advance {
			return nil
		}
		return i.advanceGeneration(old, sourceGeneration)
	}
	gen, err := nextGeneration(old)
	if err != nil {
		return err
	}
	shards := prepare(old.shards, changed, int(i.config.MaxShardBytes/recordBytes), &i.scratch, i.file != nil)
	if len(shards) == len(old.shards) {
		same := true
		for n, s := range shards {
			if s != old.shards[n] {
				same = false
				break
			}
		}
		if same && !advance {
			return nil
		} // deleting absent IDs does not commit a new root
	}
	return i.publish(shards, gen, sourceGeneration)
}

// advanceGeneration publishes unchanged shards at the next generation. It is
// the shared tail for empty and no-op batches that still advance the source
// watermark. Unchanged shards are reused, so this never rewrites payloads.
func (i *Index) advanceGeneration(old *snapshot, sourceGeneration uint64) error {
	gen, err := nextGeneration(old)
	if err != nil {
		return err
	}
	return i.publish(old.shards, gen, sourceGeneration)
}

// nextGeneration reports the generation following published state, refusing to
// wrap the uint64 generation counter.
func nextGeneration(old *snapshot) (uint64, error) {
	if old.generation == ^uint64(0) {
		return 0, ErrInvalidConfig
	}
	return old.generation + 1, nil
}
