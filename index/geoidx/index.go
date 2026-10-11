package geoidx

import (
	"cmp"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Config configures an index.
type Config struct {
	// Namespace labels the collection for the higher-level search planner. It
	// has no effect on storage layout and is supplied again on Open.
	Namespace common.Namespace
	// MaxCells is the default cover cell budget for region queries that do not
	// specify one. Values <= 0 select DefaultMaxCells.
	MaxCells int
	// TargetDelta is the expected active-delta batch size; write-path scratch is
	// sized for it. Values <= 0 select optimizedBatchEntries (64).
	TargetDelta int
	// CompactionTrigger is the minimum active-delta size that starts a
	// background compaction. The effective trigger is at least a fraction of the
	// base size (base/8) so write amplification stays bounded; a positive value
	// sets a floor. Values <= 0 select defaultCompactionTrigger (4096).
	CompactionTrigger int
	// MaxDelta is the hard pending-mutation bound. When the active plus frozen
	// delta reaches it while a compaction is running, the writer waits for that
	// compaction instead of growing without bound. Values <= 0 select
	// 2*CompactionTrigger.
	MaxDelta int
	// TargetShardBytes is the preferred payload size of one shard; a merge or
	// split aims for this. Values <= 0 select defaultTargetShardBytes (16 MiB).
	TargetShardBytes int64
	// MaxShardBytes is the payload size above which a shard is split. It bounds
	// the maximum single rewrite. Values <= 0 select defaultMaxShardBytes
	// (32 MiB).
	MaxShardBytes int64
	// MinShardBytes is the payload below which adjacent shards are candidates for
	// merging. Values <= 0 select defaultMinShardBytes (4 MiB).
	MinShardBytes int64
	// MaxFileBytes is the fixed capacity of the single persistent file
	// (geoidx.dat), created sparse at this size and reused through compaction.
	// Values <= 0 select defaultMaxFileBytes (1 GiB). Capacity is fixed at
	// creation; changing it requires rebuilding the index.
	MaxFileBytes int64
}

const (
	optimizedBatchEntries    = 64
	defaultCompactionTrigger = 4096

	defaultTargetShardBytes = 16 << 20 // 16 MiB
	defaultMaxShardBytes    = 32 << 20 // 32 MiB
	defaultMinShardBytes    = 4 << 20  // 4 MiB
	defaultMaxFileBytes     = 1 << 30  // 1 GiB fixed arena
)

func (c Config) withDefaults() (Config, error) {
	if c.MaxCells < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.MaxCells == 0 {
		c.MaxCells = DefaultMaxCells
	}
	if c.TargetDelta <= 0 {
		c.TargetDelta = optimizedBatchEntries
	}
	if c.CompactionTrigger <= 0 {
		c.CompactionTrigger = defaultCompactionTrigger
	}
	if c.MaxDelta <= 0 {
		c.MaxDelta = 2 * c.CompactionTrigger
	}
	if c.TargetShardBytes <= 0 {
		c.TargetShardBytes = defaultTargetShardBytes
	}
	if c.MaxShardBytes <= 0 {
		c.MaxShardBytes = defaultMaxShardBytes
	}
	if c.MinShardBytes <= 0 {
		c.MinShardBytes = defaultMinShardBytes
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = defaultMaxFileBytes
	}
	if c.MaxDelta < c.CompactionTrigger || c.CompactionTrigger < c.TargetDelta {
		return Config{}, ErrInvalidConfig
	}
	if c.MinShardBytes > c.TargetShardBytes || c.TargetShardBytes > c.MaxShardBytes {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// recordsForBytes converts a shard byte budget to a record count.
func recordsForBytes(bytes int64) int {
	if bytes < shardBytesPerRecord {
		return 1
	}
	return int(bytes / shardBytesPerRecord)
}

// mutation is one insert or delete. id is authoritative; key is the asserted
// location. Mutation layers are kept sorted by id and never mutated after
// publication.
type mutation struct {
	id  common.ImageID
	key S2Key
	add bool
}

// mutationSet is an id membership test over a mutation layer sorted by id. It
// stores a dense bitset when the ImageID domain is compact relative to the data,
// and otherwise falls back to a binary search over the layer. The read path
// caches one per view and compaction builds one per base rewrite, so a
// membership test is O(1) on the common domains instead of O(log delta).
type mutationSet struct {
	layer []mutation
	bits  []uint64
}

// newMutationSet builds the cheapest membership structure for layer. bound is the
// number of base entries the set will be tested against; the bitset is kept only
// while it stays proportional to that data, so sparse ImageID domains do not
// allocate a huge bitset.
func newMutationSet(layer []mutation, bound int) mutationSet {
	set := mutationSet{layer: layer}
	limit := common.ImageID(0)
	for n := range layer {
		if layer[n].id > limit {
			limit = layer[n].id
		}
	}
	if limit != 0 && uint64(limit) <= bitsetMaxID &&
		uint64(limit) <= 64*uint64(bound+len(layer)+1) {
		words := int(uint64(limit)>>6) + 1
		bits := make([]uint64, words)
		for n := range layer {
			bits[layer[n].id>>6] |= 1 << (layer[n].id & 63)
		}
		set.bits = bits
	}
	return set
}

// touched reports whether id is named by the set's layer.
func (s mutationSet) touched(id common.ImageID) bool {
	if s.bits != nil {
		word := id >> 6
		if int(word) < len(s.bits) {
			return s.bits[word]&(1<<(id&63)) != 0
		}
		return false
	}
	_, touched := mutationIndex(s.layer, id)
	return touched
}

// view is one immutable reader snapshot. Its shard slice and mutation layers are
// never mutated after publication; a writer replaces the whole view. active and
// layers are the live pending mutations (active is newest), and frozen is the
// compaction snapshot. delta is the lazily merged effective delta, computed once
// per view.
type view struct {
	shards  []*shard
	frozen  []mutation
	layers  [][]mutation
	active  []mutation
	maxID   common.ImageID
	entries int

	deltaOnce sync.Once
	delta     []mutation
	deltaAdds []mutation // effective inserts, sorted by (key, id)
	deltaSet  mutationSet
}

// Index is a specialized geographic index: lock-free readers and one serialized
// writer. It is safe for concurrent use by any number of readers and one writer.
type Index struct {
	config Config

	mu      sync.Mutex
	view    atomic.Pointer[view]
	closed  atomic.Bool
	tracker storage.ReaderTracker

	// Writer-owned state, touched only under mu.
	shards   []*shard
	active   []mutation
	layers   [][]mutation
	frozen   []mutation
	deltaMax common.ImageID
	gen      uint64

	// lastScannedShards is the number of shards whose base ImageIDs were scanned
	// to find delta-affected shards in the last prepare. Writer-owned; exposed for
	// tests and benchmarks to measure pruning effectiveness.
	lastScannedShards int

	compacting     bool
	compactionDone chan struct{}
	compactionErr  error

	persistent bool
	path       string
	lock       *os.File

	// Persistent arena state, touched only under mu (except the immutable arena
	// data, which readers access lock-free). The current mapping is never
	// re-mapped in place; growth adds a new mapping and retires the old one until
	// no published shard references it and prior readers have drained.
	arena      *region
	retired    []*region
	alloc      *pagealloc.Allocator
	live       []extentRef // durable live set of the two committed generations
	slot       int
	arenaBytes uint64
	dirOffset  uint64
	dirLength  uint32
	migrateAll bool
}

// NewMemory returns an empty heap-backed index.
func NewMemory(config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	index := &Index{config: cfg, gen: 1}
	index.publishLocked()
	return index, nil
}

// maxImageID returns the largest ImageID across the base bound and the pending
// mutation state.
func maxImageID(baseMax common.ImageID, frozen []mutation, layers [][]mutation, active []mutation) common.ImageID {
	best := baseMax
	maxLayer := func(layer []mutation) {
		for n := range layer {
			if layer[n].id > best {
				best = layer[n].id
			}
		}
	}
	maxLayer(frozen)
	for _, layer := range layers {
		maxLayer(layer)
	}
	maxLayer(active)
	return best
}

// mergeAll merges the pending mutation layers into one id-sorted effective delta
// (frozen is oldest, active is newest; newer layers win).
func mergeAll(frozen []mutation, layers [][]mutation, active []mutation) []mutation {
	out := frozen
	for _, layer := range layers {
		out = mergeMutations(out, layer)
	}
	return mergeMutations(out, active)
}

// viewDelta returns the effective mutation delta, or nil when there are no
// pending mutations. The merge, a key-sorted insert list, and a membership
// bitset are computed once per view and shared by all readers of that view.
func viewDelta(v *view) []mutation {
	if len(v.frozen) == 0 && len(v.layers) == 0 && len(v.active) == 0 {
		return nil
	}
	v.deltaOnce.Do(func() {
		v.delta = mergeAll(v.frozen, v.layers, v.active)
		adds := make([]mutation, 0, len(v.delta))
		for n := range v.delta {
			if v.delta[n].add {
				adds = append(adds, v.delta[n])
			}
		}
		slices.SortFunc(adds, func(a, b mutation) int {
			if order := cmp.Compare(a.key, b.key); order != 0 {
				return order
			}
			return cmp.Compare(a.id, b.id)
		})
		v.deltaAdds = adds
		v.deltaSet = newMutationSet(v.delta, v.entries)
	})
	return v.delta
}

// deltaTouched reports whether id is named by the effective delta, using the
// cached membership set.
func deltaTouched(v *view, id common.ImageID) bool {
	return v.deltaSet.touched(id)
}

func (i *Index) publishLocked() {
	// The layer slice is copied so the writer can keep appending/merging while
	// readers iterate the published snapshot. The shard slice is replaced
	// wholesale by compaction (never appended to in place), so it can be shared.
	layers := append([][]mutation(nil), i.layers...)
	maxID := i.deltaMax
	entries := 0
	for _, s := range i.shards {
		entries += s.count()
		if s.maxID > maxID {
			maxID = s.maxID
		}
	}
	i.view.Store(&view{
		shards:  i.shards,
		frozen:  i.frozen,
		layers:  layers,
		active:  i.active,
		maxID:   maxID,
		entries: entries,
	})
}

// withView runs fn while pinning the current view so compaction and Close cannot
// reclaim or unmap an arena the reader may still touch. The order is strict:
// Acquire -> check closed -> use view -> Release.
func (i *Index) withView(fn func(*view)) {
	token := i.tracker.Acquire()
	if !i.closed.Load() {
		fn(i.view.Load())
	}
	i.tracker.Release(token)
}

// Insert records id at key.
func (i *Index) Insert(key S2Key, id common.ImageID) error {
	var ops [1]mutation
	ops[0] = mutation{id: id, key: key, add: true}
	return i.apply(ops[:])
}

// Delete removes id. Deletion is by ImageID: an image has at most one location,
// so the supplied key is advisory and a stale key does not prevent removal. A
// move is Delete(oldKey, id) then Insert(newKey, id); the caller supplies
// oldKey from imagemetadata.
func (i *Index) Delete(key S2Key, id common.ImageID) error {
	var ops [1]mutation
	ops[0] = mutation{id: id, key: key, add: false}
	return i.apply(ops[:])
}

// InsertBatch records every entry as an insert.
func (i *Index) InsertBatch(entries []Entry) error {
	return i.applyEntries(entries, true)
}

// DeleteBatch removes every entry.
func (i *Index) DeleteBatch(entries []Entry) error {
	return i.applyEntries(entries, false)
}

// Apply records inserts and then deletes; a later operation for an ImageID wins.
func (i *Index) Apply(inserts, deletes []Entry) error {
	total := len(inserts) + len(deletes)
	var stack [2 * optimizedBatchEntries]mutation
	var ops []mutation
	if total <= len(stack) {
		ops = stack[:0]
	} else {
		ops = make([]mutation, 0, total)
	}
	for _, e := range inserts {
		ops = append(ops, mutation{id: e.ImageID, key: e.Key, add: true})
	}
	for _, e := range deletes {
		ops = append(ops, mutation{id: e.ImageID, key: e.Key, add: false})
	}
	return i.apply(ops)
}

// applyEntries stages an entry batch on the stack for the common <=64 case,
// falling back to a heap slice only for larger batches.
func (i *Index) applyEntries(entries []Entry, add bool) error {
	var stack [optimizedBatchEntries]mutation
	var ops []mutation
	if len(entries) <= len(stack) {
		ops = stack[:len(entries)]
	} else {
		ops = make([]mutation, len(entries))
	}
	for n, e := range entries {
		ops[n] = mutation{id: e.ImageID, key: e.Key, add: add}
	}
	return i.apply(ops)
}

// apply admits and publishes one mutation batch. Admission runs before
// publication: a batch that would push the pending delta over the hard budget
// first waits for a compaction (outside the writer mutex); if no compaction can
// make progress it returns ErrBackpressure without publishing, so a successful
// return always means the batch was published. A single batch larger than the
// budget is admitted when the delta is otherwise empty (bulk load), after which
// compaction bounds growth to one batch.
func (i *Index) apply(ops []mutation) error {
	if len(ops) == 0 {
		return nil
	}
	for _, op := range ops {
		if op.id == 0 {
			return ErrInvalidID
		}
		if _, err := CellIDFromKey(op.key); err != nil {
			return err
		}
	}

	i.mu.Lock()
	for {
		if i.closed.Load() {
			i.mu.Unlock()
			return ErrClosed
		}
		pending := i.pendingLocked()
		if pending == 0 || pending+len(ops) <= i.hardDelta() {
			break
		}
		if !i.compacting {
			if pending == 0 {
				i.mu.Unlock()
				return ErrBackpressure
			}
			i.startCompactionLocked()
			if !i.compacting {
				i.mu.Unlock()
				return ErrBackpressure
			}
		}
		done := i.compactionDone
		i.mu.Unlock()
		<-done
		i.mu.Lock()
		if i.compactionErr != nil {
			err := i.compactionErr
			i.compactionErr = nil
			if i.pendingLocked()+len(ops) > i.hardDelta() {
				i.mu.Unlock()
				return errors.Join(ErrBackpressure, err)
			}
		}
	}
	for _, op := range ops {
		if op.id > i.deltaMax {
			i.deltaMax = op.id
		}
	}
	switch {
	case len(ops) <= optimizedBatchEntries:
		next := make([]mutation, len(i.active), len(i.active)+len(ops))
		copy(next, i.active)
		for _, op := range ops {
			next = upsertMutation(next, op)
		}
		i.active = next
	default:
		// Large batch: sort once and merge, so building the delta is O(n+m)
		// instead of an O(n) insert per operation. ops is a fresh slice here.
		if !slices.IsSortedFunc(ops, compareMutationID) {
			slices.SortStableFunc(ops, compareMutationID)
		}
		i.active = mergeMutations(i.active, lastWins(ops))
	}
	if len(i.active) >= i.config.TargetDelta {
		i.freezeLocked()
	}
	i.publishLocked()
	if !i.compacting && i.shouldCompactLocked() {
		i.startCompactionLocked()
	}
	i.mu.Unlock()
	return nil
}

func compareMutationID(a, b mutation) int { return cmp.Compare(a.id, b.id) }

// lastWins collapses a slice sorted by id to one mutation per id, keeping the
// last occurrence (the write path's later-operation-wins rule).
func lastWins(sorted []mutation) []mutation {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for n := 1; n < len(sorted); n++ {
		if sorted[n].id == out[len(out)-1].id {
			out[len(out)-1] = sorted[n]
			continue
		}
		out = append(out, sorted[n])
	}
	return out
}

// mergeMutations merges two id-sorted mutation slices; ops (the new batch) wins
// on equal ids.
func mergeMutations(base, ops []mutation) []mutation {
	out := make([]mutation, 0, len(base)+len(ops))
	b, o := 0, 0
	for b < len(base) || o < len(ops) {
		switch {
		case o >= len(ops) || (b < len(base) && base[b].id < ops[o].id):
			out = append(out, base[b])
			b++
		case b >= len(base) || ops[o].id < base[b].id:
			out = append(out, ops[o])
			o++
		default:
			out = append(out, ops[o]) // ops wins
			b++
			o++
		}
	}
	return out
}

func upsertMutation(muts []mutation, op mutation) []mutation {
	index, found := mutationIndex(muts, op.id)
	if found {
		muts[index].key = op.key
		muts[index].add = op.add
		return muts
	}
	return slices.Insert(muts, index, op)
}

func mutationIndex(muts []mutation, id common.ImageID) (int, bool) {
	return slices.BinarySearchFunc(muts, id, func(m mutation, id common.ImageID) int {
		return cmp.Compare(m.id, id)
	})
}

// pendingLocked returns the number of pending mutations.
func (i *Index) pendingLocked() int {
	n := len(i.active) + len(i.frozen)
	for _, layer := range i.layers {
		n += len(layer)
	}
	return n
}

// freezeLocked moves the active delta into the layer set and geometrically merges
// equal-sized adjacent layers, so writes stay O(TargetDelta) amortized and the
// layer count stays O(log delta).
func (i *Index) freezeLocked() {
	if len(i.active) == 0 {
		return
	}
	i.layers = append(i.layers, i.active)
	i.active = nil
	for len(i.layers) >= 2 {
		last := len(i.layers) - 1
		if len(i.layers[last-1]) > len(i.layers[last]) {
			break
		}
		merged := mergeMutations(i.layers[last-1], i.layers[last])
		i.layers = i.layers[:last-1]
		i.layers = append(i.layers, merged)
	}
}

// compactionTrigger returns the pending-delta size that starts a background
// compaction: at least base/8, so write amplification stays bounded, and at
// least the configured floor. It is evaluated against the whole sharded base, so
// the per-mutation rewrite ratio is unchanged by sharding; sharding bounds the
// size of each individual rewrite instead.
func (i *Index) compactionTrigger() int {
	trigger := i.config.CompactionTrigger
	if fraction := i.baseEntriesLocked() / 8; fraction > trigger {
		trigger = fraction
	}
	return trigger
}

// baseEntriesLocked sums the committed record counts. The caller holds mu.
func (i *Index) baseEntriesLocked() int {
	n := 0
	for _, s := range i.shards {
		n += s.count()
	}
	return n
}

// maxShardIDLocked returns the largest ImageID among committed shards. The caller
// holds mu.
func (i *Index) maxShardIDLocked() common.ImageID {
	var best common.ImageID
	for _, s := range i.shards {
		if s.maxID > best {
			best = s.maxID
		}
	}
	return best
}

// hardDelta caps pending mutations; the writer waits for compaction at the
// bound instead of growing without limit.
func (i *Index) hardDelta() int {
	if hard := 2 * i.compactionTrigger(); hard > i.config.MaxDelta {
		return hard
	}
	return i.config.MaxDelta
}

func (i *Index) shouldCompactLocked() bool {
	return len(i.frozen) != 0 || i.pendingLocked() >= i.compactionTrigger()
}

// startCompactionLocked snapshots the pending layers into one frozen layer and
// launches a background compaction. The writer is not blocked by the base
// rewrite; new writes accumulate in fresh layers.
func (i *Index) startCompactionLocked() {
	if i.compacting {
		return
	}
	if len(i.frozen) == 0 {
		if len(i.layers) == 0 && len(i.active) == 0 {
			return
		}
		i.frozen = mergeAll(nil, i.layers, i.active)
		i.layers = nil
		i.active = nil
	}
	i.compacting = true
	i.compactionDone = make(chan struct{})
	i.publishLocked()
	go i.runCompaction()
}

// Compact folds all pending mutations into the base synchronously and returns
// when done. It is the explicit durability point; ordinary small batches only
// schedule background compaction.
func (i *Index) Compact() error {
	for {
		i.mu.Lock()
		if i.closed.Load() {
			i.mu.Unlock()
			return ErrClosed
		}
		if !i.compacting {
			if i.pendingLocked() == 0 {
				i.mu.Unlock()
				return nil
			}
			i.startCompactionLocked()
		}
		done := i.compactionDone
		i.mu.Unlock()
		<-done
		i.mu.Lock()
		if i.compactionErr != nil {
			err := i.compactionErr
			i.compactionErr = nil
			i.mu.Unlock()
			return err
		}
		i.mu.Unlock()
	}
}

// Checkpoint makes pending mutations durable. It is a no-op for a memory-backed
// index. Close does not checkpoint.
func (i *Index) Checkpoint() error {
	i.mu.Lock()
	persistent := i.persistent
	closed := i.closed.Load()
	i.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !persistent {
		return nil
	}
	return i.Compact()
}

// Sync is an alias for Checkpoint, matching the other index packages.
func (i *Index) Sync() error { return i.Checkpoint() }

// Generation returns the current committed directory generation.
func (i *Index) Generation() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.gen
}

// Namespace returns the namespace configured for this index.
func (i *Index) Namespace() common.Namespace { return i.config.Namespace }

// Len returns the effective number of stored locations (base minus deletes plus
// inserts).
func (i *Index) Len() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return 0
	}
	// Collapse the layers into one effective delta, then count.
	v := i.view.Load()
	viewDelta(v)
	count := uint64(0)
	for _, s := range v.shards {
		for n := range s.keys {
			if !v.deltaSet.touched(s.ids[n]) {
				count++
			}
		}
	}
	for n := range v.delta {
		if v.delta[n].add {
			count++
		}
	}
	return count
}
