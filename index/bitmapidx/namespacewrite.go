package bitmapidx

import (
	"bytes"
	"slices"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
	"github.com/evanoberholster/imageIdx/roar"
)

// prepare canonicalizes values, computes hash-level keys, and groups
// changes by (base hash, canonical bytes). It runs before the writer lock
// so analyzer and hashing work stays concurrent. Returned canonical
// slices borrow caller input and are copied under the lock only for
// genuinely new dictionary values. Owner pre-checks are conservative:
// entry counts scale by batchFanoutBound and byte budgets cap source
// bytes; the WAL encoder enforces exact membership limits later.
func (i *Index) prepare(namespace NamespaceSpec, mutation namespaceMutation) ([]stagedChange, error) {
	if len(mutation.Add) == 0 && len(mutation.Remove) == 0 {
		return nil, nil
	}
	if i.owner != nil {
		if len(mutation.Add) > MaxNamespaceBatchUpdates*batchFanoutBound || len(mutation.Remove) > MaxNamespaceBatchUpdates*batchFanoutBound-len(mutation.Add) {
			return nil, ErrBatchTooLarge
		}
		remaining := MaxNamespaceBatchBytes
		for _, entries := range [][]Entry{mutation.Add, mutation.Remove} {
			for _, entry := range entries {
				if len(entry.Value.Bytes) > remaining {
					return nil, ErrBatchTooLarge
				}
				remaining -= len(entry.Value.Bytes)
			}
		}
	}
	total := len(mutation.Add) + len(mutation.Remove)
	// Tiny batches group by a linear scan; a map only pays off for larger
	// batches. The map keys hash-level base keys and chains same-hash
	// canonical variants through stagedChange.next.
	const linearGroupBound = 8
	useMap := total > linearGroupBound
	var position map[NamespaceKey]int
	if useMap {
		position = make(map[NamespaceKey]int, total)
	}
	changes := make([]stagedChange, 0, total)
	stage := func(entry Entry, add bool) error {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		key, canonical, err := i.keyForValue(namespace, entry.Value)
		if err != nil {
			return err
		}
		index := -1
		if useMap {
			if at, ok := position[key]; ok {
				index = at
				for {
					if bytes.Equal(changes[index].canonical, canonical) {
						break
					}
					if changes[index].next < 0 {
						changes = append(changes, stagedChange{base: key, canonical: canonical, next: -1})
						changes[index].next = len(changes) - 1
						index = len(changes) - 1
						break
					}
					index = changes[index].next
				}
			}
		} else {
			for at := range changes {
				if changes[at].base == key && bytes.Equal(changes[at].canonical, canonical) {
					index = at
					break
				}
			}
		}
		if index < 0 {
			changes = append(changes, stagedChange{base: key, canonical: canonical, next: -1})
			index = len(changes) - 1
			if useMap {
				position[key] = index
			}
		}
		if add {
			changes[index].add = append(changes[index].add, entry.ImageID)
		} else {
			changes[index].remove = append(changes[index].remove, entry.ImageID)
		}
		return nil
	}
	for _, entry := range mutation.Add {
		if err := stage(entry, true); err != nil {
			return nil, err
		}
	}
	for _, entry := range mutation.Remove {
		if err := stage(entry, false); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// keyForValue canonicalizes one value and returns its hash-level key.
// canonical is nil for continuous and geo values, which have no
// dictionary entry; every other kind returns bytes that must resolve to a
// slotted key. Empty enum bytes normalize to an empty non-nil slice.
func (i *Index) keyForValue(namespace NamespaceSpec, value Value) (NamespaceKey, []byte, error) {
	switch namespace.Kind {
	case common.KindBitmapEnum:
		if value.Kind != common.KindBitmapEnum {
			return NamespaceKey{}, nil, ErrValueKind
		}
		canonical, err := normalizeEnum(namespace.Normalizer, value.Bytes)
		if err != nil {
			return NamespaceKey{}, nil, err
		}
		if len(canonical) > MaxValueBytes {
			return NamespaceKey{}, nil, ErrValueTooLong
		}
		if canonical == nil {
			canonical = []byte{}
		}
		return hashCanonical(namespace.Namespace, common.KindBitmapEnum, canonical), canonical, nil
	case common.KindBitmapBool:
		if value.Kind != common.KindBitmapBool {
			return NamespaceKey{}, nil, ErrValueKind
		}
		canonical := []byte{'0'}
		if value.Bool {
			canonical[0] = '1'
		}
		return hashCanonical(namespace.Namespace, common.KindBitmapBool, canonical), canonical, nil
	case common.KindBitmapString:
		if value.Kind != common.KindBitmapString {
			return NamespaceKey{}, nil, ErrValueKind
		}
		if len(value.Bytes) > MaxValueBytes {
			return NamespaceKey{}, nil, ErrValueTooLong
		}
		return hashCanonical(namespace.Namespace, common.KindBitmapString, value.Bytes), value.Bytes, nil
	case common.KindBitmapContinuous:
		if value.Kind != common.KindBitmapContinuous {
			return NamespaceKey{}, nil, ErrValueKind
		}
		return continuousKey(namespace.Namespace, value.Int), nil, nil
	case common.KindBitmapGeo:
		if value.Kind != common.KindBitmapGeo {
			return NamespaceKey{}, nil, ErrValueKind
		}
		if value.Int < 0 {
			return NamespaceKey{}, nil, ErrInvalidS2Cell
		}
		key := uint64(value.Int)
		if _, err := CellIDFromKey(key); err != nil {
			return NamespaceKey{}, nil, err
		}
		return NamespaceKey{Hash: key, Namespace: namespace.Namespace}, nil, nil
	default:
		return NamespaceKey{}, nil, ErrInvalidNamespace
	}
}

// applyChangesReplay republishes WAL-decoded changes: replay serializes
// dictionary resolution, delta composition, and view publication for one
// decoded frame, without live geo preconditions or WAL framing. Live writes
// commit through the group pipeline in commit.go instead. It runs before the
// store assigns ownership, so it takes only the index writer lock.
//
// Every fallible step completes before publication; on failure the dictionary
// writer rolls back to a chunk-cloned snapshot and aborts staged entries, so
// no phantom value survives. Rotation policy: hard limits fold into Base
// synchronously, ActiveTarget rotates Active to Frozen and schedules
// background compaction.
func (i *Index) applyChangesReplay(changes []stagedChange) error {
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	current := i.view.Load()
	saved := i.dict
	saved.chunks = slices.Clone(saved.chunks)
	committed := false
	defer func() {
		if !committed {
			i.dict = saved
			i.dict.abort()
		}
	}()
	resolved, _, _, err := i.resolveBatch(current, changes, true)
	if err != nil {
		return err
	}
	active := composeNamespaceDelta(current, resolved)
	if active == nil && current.active == nil {
		return nil // every requested change already matched published state
	}
	base, frozen, active, rotate := i.planRotation(current, active)
	i.dict.seal()
	// Every fallible step is done; publication cannot fail.
	i.view.Store(i.nextView(current, base, frozen, active))
	committed = true
	if rotate {
		i.scheduleCompaction(frozen)
	}
	return nil
}

// resolveBatch resolves and validates one prepared batch under writerMu and
// freezes its WAL frame for store-owned indexes. It performs every fallible
// step before the caller appends to the WAL; a failure rolls the dictionary
// writer back through the caller's deferred restore.
func (i *Index) resolveBatch(current *namespaceView, changes []stagedChange, replay bool) (resolved []stagedChange, walPayload []byte, walUpdates uint32, err error) {
	resolved, err = i.resolveChanges(current, changes)
	if err != nil {
		return nil, nil, 0, err
	}
	if !replay {
		if err := validateGeoChanges(current, resolved); err != nil {
			return nil, nil, 0, err
		}
	}
	if i.owner != nil {
		// Bound and freeze the canonical frame before allocating replacement
		// postings. Encoding also removes duplicate/conflicting memberships.
		walPayload, walUpdates, err = encodeNamespaceChanges(resolved)
		if err != nil {
			return nil, nil, 0, err
		}
	}
	return resolved, walPayload, walUpdates, nil
}

// planRotation applies the delta rotation policy and returns the Base,
// Frozen, and Active layers for the next view. Hard limits fold Frozen and
// Active into a replacement Base synchronously; otherwise a full Active
// rotates to Frozen and the caller schedules background compaction.
func (i *Index) planRotation(current *namespaceView, active *namespaceDelta) (base postingTable, frozen, next *namespaceDelta, rotate bool) {
	switch {
	case active != nil && (active.ids >= i.config.HardDeltaLimit || len(active.values) > i.config.MaxDeltaValues):
		return current.effectiveTable(current.frozen, active), nil, nil, false
	case current.frozen == nil && active != nil && active.ids >= i.config.ActiveTarget:
		return current.base, active, nil, true
	default:
		return current.base, current.frozen, active, false
	}
}

// resolveChanges maps prepared values to dictionary keys with slots.
// Additions claim a collision slot and stage bytes; removal-only changes
// with no matching value are dropped as no-ops. Continuous and geo changes
// (nil canonical) pass through with their base key.
//
// It compacts the slice in place: the write cursor never overtakes the read
// cursor because at most one output is produced per input, and each read
// happens before the corresponding write, so reusing changes' backing is
// safe and allocation-free. The returned slice aliases changes' storage.
func (i *Index) resolveChanges(view *namespaceView, changes []stagedChange) ([]stagedChange, error) {
	out := changes[:0]
	for read := range changes {
		change := changes[read]
		if change.canonical == nil {
			change.key = change.base
			out = append(out, change)
			continue
		}
		if len(change.add) != 0 {
			key, err := i.dict.assign(view.dict, change.base, change.canonical)
			if err != nil {
				return nil, err
			}
			change.key = key
			out = append(out, change)
			continue
		}
		key, ok := view.dict.resolve(change.base, change.canonical)
		if !ok {
			if key, ok = i.dict.resolvePending(change.base, change.canonical); !ok {
				continue // unknown removal: never creates a dictionary entry
			}
		}
		change.key = key
		out = append(out, change)
	}
	return out, nil
}

// composeNamespaceDelta resolves the batch against the current view. It
// shallow-copies Active (untouched deltas stay shared) and recomputes
// only touched keys. IDs already named in Active keep their staged state
// without rechecking lower layers; only genuinely new IDs probe
// containsWithoutActive. Remove wins within the batch. It returns nil when
// the batch matches published state.
func composeNamespaceDelta(view *namespaceView, changes []stagedChange) *namespaceDelta {
	var values map[NamespaceKey]*bitmapDelta
	var keys []NamespaceKey
	ids := 0
	if view.active != nil {
		values = make(map[NamespaceKey]*bitmapDelta, len(view.active.values)+len(changes))
		for key, bd := range view.active.values {
			values[key] = bd
		}
		keys = slices.Clone(view.active.keys)
		ids = view.active.ids
	} else {
		values = make(map[NamespaceKey]*bitmapDelta, len(changes))
	}

	for _, change := range changes {
		adds := dedupeSortedIDs(change.add)
		rems := dedupeSortedIDs(change.remove)
		adds = subtractSortedIDs(adds, rems) // remove wins within the batch

		old, _ := view.active.delta(change.key)
		var oldAdds, oldRems []common.ImageID
		oldLen := 0
		if old != nil {
			if !old.additions.IsEmpty() {
				oldAdds = old.additions.AppendTo(nil)
			}
			if !old.removals.IsEmpty() {
				oldRems = old.removals.AppendTo(nil)
			}
			oldLen = old.len()
		}

		// Corrections that stay valid keep their state without rechecking the
		// underlying layers: removals carry through, additions carry through.
		stayingAdds := subtractSortedIDs(oldAdds, rems)
		stayingRems := subtractSortedIDs(oldRems, adds)
		var newAdds, newRems []common.ImageID
		for _, id := range adds {
			// Already added: underlying lacks the ID. Covered by stayingAdds.
			if containsSortedID(oldAdds, id) {
				continue
			}
			// Previously removed: underlying holds the ID, so an addition
			// would match published state and must not be recorded.
			if containsSortedID(oldRems, id) {
				continue
			}
			if !view.containsWithoutActive(change.key, id) {
				newAdds = append(newAdds, id)
			}
		}
		for _, id := range rems {
			// Already removed: covered by stayingRems.
			if containsSortedID(oldRems, id) {
				continue
			}
			// Previously added: underlying lacks the ID, so dropping the
			// addition is enough.
			if containsSortedID(oldAdds, id) {
				continue
			}
			if view.containsWithoutActive(change.key, id) {
				newRems = append(newRems, id)
			}
		}
		addFinal := mergeSortedIDs(stayingAdds, newAdds)
		remFinal := mergeSortedIDs(stayingRems, newRems)
		if len(addFinal) == 0 && len(remFinal) == 0 {
			if old != nil {
				delete(values, change.key)
				if index, ok := slices.BinarySearchFunc(keys, change.key, compareNamespaceKey); ok {
					keys = slices.Delete(keys, index, index+1)
				}
				ids -= oldLen
			}
			continue
		}
		values[change.key] = &bitmapDelta{
			additions: roar.FromUnsorted(addFinal),
			removals:  roar.FromUnsorted(remFinal),
		}
		if old == nil {
			index, _ := slices.BinarySearchFunc(keys, change.key, compareNamespaceKey)
			keys = slices.Insert(keys, index, change.key)
		}
		ids += len(addFinal) + len(remFinal) - oldLen
	}
	if len(values) == 0 {
		return nil
	}
	return &namespaceDelta{values: values, keys: keys, ids: ids}
}

// dedupeSortedIDs sorts ids in place and returns the unique prefix. The
// input must be caller-owned; encode and compose both rely on sorted,
// deduplicated columns.
func dedupeSortedIDs(ids []common.ImageID) []common.ImageID {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// containsSortedID binary-searches an ascending slice.
func containsSortedID(ids []common.ImageID, id common.ImageID) bool {
	_, ok := slices.BinarySearch(ids, id)
	return ok
}

// subtractSortedIDs returns values minus minus, both ascending and
// unique. It reuses values backing when possible; callers must own values.
func subtractSortedIDs(values, minus []common.ImageID) []common.ImageID {
	if len(values) == 0 || len(minus) == 0 {
		return values
	}
	out := values[:0]
	keep, drop := 0, 0
	for keep < len(values) && drop < len(minus) {
		switch {
		case values[keep] < minus[drop]:
			out = append(out, values[keep])
			keep++
		case values[keep] > minus[drop]:
			drop++
		default:
			keep++
			drop++
		}
	}
	return append(out, values[keep:]...)
}

// mergeSortedIDs returns the ascending union of two ascending, unique
// slices. Empty inputs are returned without allocating.
func mergeSortedIDs(first, second []common.ImageID) []common.ImageID {
	if len(first) == 0 {
		return second
	}
	if len(second) == 0 {
		return first
	}
	out := make([]common.ImageID, 0, len(first)+len(second))
	left, right := 0, 0
	for left < len(first) && right < len(second) {
		switch {
		case first[left] < second[right]:
			out = append(out, first[left])
			left++
		case first[left] > second[right]:
			out = append(out, second[right])
			right++
		default:
			out = append(out, first[left])
			left++
			right++
		}
	}
	out = append(out, first[left:]...)
	return append(out, second[right:]...)
}

// publishLocked publishes a replacement view. The caller holds writerMu.
func (i *Index) publishLocked(old *namespaceView, base postingTable, frozen, active *namespaceDelta) *namespaceView {
	next := i.nextView(old, base, frozen, active)
	i.view.Store(next)
	return next
}

func (i *Index) nextView(old *namespaceView, base postingTable, frozen, active *namespaceDelta) *namespaceView {
	return &namespaceView{
		namespace:  old.namespace,
		base:       base,
		frozen:     frozen,
		active:     active,
		dict:       i.dict.view(),
		dictGen:    i.dict.generation,
		generation: old.generation + 1,
	}
}

// scheduleCompaction folds a rotated Frozen into Base in the background.
// It captures the frozen pointer and publishes only if it is still
// current, so superseded rotations are skipped safely.
func (i *Index) scheduleCompaction(frozen *namespaceDelta) {
	i.workers.Add(1)
	i.pending.Add(1)
	go func() {
		defer i.workers.Done()
		defer i.pending.Add(-1)
		if i.config.CompactDelay != 0 {
			time.Sleep(i.config.CompactDelay)
		}
		token := i.readers.Acquire()
		initial := i.view.Load()
		if i.closed.Load() || initial.frozen != frozen {
			i.readers.Release(token)
			return
		}
		base := initial.effectiveTable(frozen)
		i.writerMu.Lock()
		current := i.view.Load()
		if current.frozen == frozen && !i.closed.Load() {
			i.publishLocked(current, base, nil, current.active)
		}
		i.writerMu.Unlock()
		i.readers.Release(token)
		i.readers.WaitForPriorReaders()
	}()
}

// rebuildDictionary rewrites dictionary chunks with only entries that
// still have a posting in base. The caller holds writerMu. The local
// rebuild stages, seals, and compacts, then swaps into the live writer
// with one generation bump; the local seal's generation is discarded.
func (i *Index) rebuildDictionary(view *namespaceView, base postingTable) {
	live := make(map[NamespaceKey]struct{}, len(base.keys))
	for _, key := range base.keys {
		live[key] = struct{}{}
	}
	var rebuilt dictionaryWriter
	for _, entry := range view.dict.merged() {
		if _, ok := live[entry.key]; !ok {
			continue
		}
		rebuilt.stage(entry.key, view.dict.bytes(entry))
	}
	rebuilt.seal()
	rebuilt.compact()
	i.dict.rebuild(rebuilt.base, rebuilt.chunks)
}

// analyzeSet returns the deduplicated analyzer token set for source
// strings. The scratch token slice is reused across docs.
func analyzeSet(analyzer *textanalyzer.Analyzer, docs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	var tokens []string
	for _, doc := range docs {
		var err error
		tokens, err = analyzer.Analyze(doc, tokens[:0])
		if err != nil {
			return nil, err
		}
		for _, token := range tokens {
			out[token] = struct{}{}
		}
	}
	return out, nil
}
