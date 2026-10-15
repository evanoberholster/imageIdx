package bitmapidx

import (
	"slices"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/roar"
)

// postingTable is the immutable base of one namespace: keys sorted by
// NamespaceKey and their postings at matching positions. Slices are shared
// across publications and never mutated after publication; writers clone
// before folding deltas.
type postingTable struct {
	keys     []NamespaceKey
	postings []posting
}

func (t *postingTable) get(key NamespaceKey) (*posting, bool) {
	index, ok := slices.BinarySearchFunc(t.keys, key, func(e NamespaceKey, k NamespaceKey) int {
		return compareNamespaceKey(e, k)
	})
	if !ok {
		return nil, false
	}
	return &t.postings[index], true
}

func (t *postingTable) clone() postingTable {
	return postingTable{keys: slices.Clone(t.keys), postings: slices.Clone(t.postings)}
}

// bitmapDelta is one immutable correction for a single namespace value:
// additions minus removals, already canonicalized so the sets are
// disjoint. Values are roaring bitmaps shared across views.
type bitmapDelta struct {
	additions roar.Bitmap
	removals  roar.Bitmap
}

// newBitmapDelta canonicalizes additions minus removals into an immutable
// correction. It returns nil when the correction is empty, so callers can
// keep delta maps sparse.
func newBitmapDelta(additions, removals []schema.ImageID) *bitmapDelta {
	if len(additions) == 0 && len(removals) == 0 {
		return nil
	}
	add := roar.FromUnsorted(additions)
	remove := roar.FromUnsorted(removals)
	builder := roar.NewDeltaBuilderFrom(add)
	builder.AndNot(remove)
	add = builder.Freeze()
	if add.IsEmpty() && remove.IsEmpty() {
		return nil
	}
	return &bitmapDelta{additions: add, removals: remove}
}

// len returns the IDs in the correction, safe on nil.
func (d *bitmapDelta) len() int {
	if d == nil {
		return 0
	}
	return int(d.additions.Cardinality() + d.removals.Cardinality())
}

// namespaceDelta is one immutable correction layer: per-key added/removed
// IDs. values holds corrections by resolved key, keys is the sorted key
// set for ordered scans, and ids is the total corrected ID count used for
// rotation decisions. Untouched entries are shared across publications.
type namespaceDelta struct {
	values map[NamespaceKey]*bitmapDelta
	keys   []NamespaceKey
	ids    int
}

func (d *namespaceDelta) delta(key NamespaceKey) (*bitmapDelta, bool) {
	if d == nil {
		return nil, false
	}
	bd, ok := d.values[key]
	return bd, ok
}

// state reports the layer's correction for (key, id), if any. Additions
// win within the layer because newBitmapDelta canonicalizes remove-wins
// before storage.
func (d *namespaceDelta) state(key NamespaceKey, id schema.ImageID) (present, touched bool) {
	if d == nil {
		return false, false
	}
	bd, ok := d.values[key]
	if !ok {
		return false, false
	}
	if bd.additions.Contains(id) {
		return true, true
	}
	if bd.removals.Contains(id) {
		return false, true
	}
	return false, false
}

// namespaceView is one immutable, atomically published snapshot of a namespace.
// Lookup precedence is active, then frozen, then base. dictGen mirrors
// the dictionary generation so compiled queries can fast-path when the
// value set is unchanged; generation orders view publications.
type namespaceView struct {
	namespace  NamespaceSpec
	base       postingTable
	frozen     *namespaceDelta
	active     *namespaceDelta
	dict       *dictionaryView
	dictGen    uint64
	generation uint64
}

func (v *namespaceView) hasDeltas() bool { return v.frozen != nil || v.active != nil }

// allKeys returns the sorted, unique keys present in the base and any delta
// layers.
func (v *namespaceView) allKeys() []NamespaceKey {
	size := len(v.base.keys)
	if v.frozen != nil {
		size += len(v.frozen.keys)
	}
	if v.active != nil {
		size += len(v.active.keys)
	}
	keys := make([]NamespaceKey, 0, size)
	keys = append(keys, v.base.keys...)
	if v.frozen != nil {
		keys = append(keys, v.frozen.keys...)
	}
	if v.active != nil {
		keys = append(keys, v.active.keys...)
	}
	slices.SortFunc(keys, compareNamespaceKey)
	return slices.Compact(keys)
}

func (v *namespaceView) contains(key NamespaceKey, id schema.ImageID) bool {
	if v.active == nil && v.frozen == nil {
		if p, ok := v.base.get(key); ok {
			return p.contains(id)
		}
		return false
	}
	if present, ok := v.active.state(key, id); ok {
		return present
	}
	if present, ok := v.frozen.state(key, id); ok {
		return present
	}
	if p, ok := v.base.get(key); ok {
		return p.contains(id)
	}
	return false
}

func (v *namespaceView) containsWithoutActive(key NamespaceKey, id schema.ImageID) bool {
	if present, ok := v.frozen.state(key, id); ok {
		return present
	}
	if p, ok := v.base.get(key); ok {
		return p.contains(id)
	}
	return false
}

// effective returns the fully corrected posting for key. The fast path
// returns the shared immutable base posting with no allocation;
// corrected keys fold frozen then active through a DeltaBuilder and demote
// small results to tiny postings. The returned pointer aliases base or a
// fresh heap posting valid while the view is retained.
func (v *namespaceView) effective(key NamespaceKey) (*posting, bool) {
	base, hasBase := v.base.get(key)
	frozen, hasFrozen := v.frozen.delta(key)
	active, hasActive := v.active.delta(key)
	if !hasFrozen && !hasActive {
		return base, hasBase
	}
	var builder *roar.DeltaBuilder
	if hasBase {
		builder = roar.NewDeltaBuilderFrom(base.bitmap())
	} else {
		builder = roar.NewDeltaBuilder()
	}
	if hasFrozen {
		builder.ApplyDelta(frozen.additions, frozen.removals)
	}
	if hasActive {
		builder.ApplyDelta(active.additions, active.removals)
	}
	out := builder.Freeze()
	if out.IsEmpty() {
		return nil, false
	}
	corrected := makePosting(out)
	return &corrected, true
}

// deltaTouches reports whether any delta layer carries a correction for key.
func (v *namespaceView) deltaTouches(key NamespaceKey) bool {
	if _, ok := v.frozen.delta(key); ok {
		return true
	}
	_, ok := v.active.delta(key)
	return ok
}

// effectiveInto writes the fully corrected posting for key into m and returns
// it, aliasing m. It mirrors effective but recycles m's buffer, so a range query
// can fold many corrected keys without allocating a fresh bitmap per key. When
// key has no correction it returns the shared base posting without touching m.
func (v *namespaceView) effectiveInto(key NamespaceKey, m *roar.MutableBitmap) (roar.Bitmap, bool) {
	base, hasBase := v.base.get(key)
	frozen, hasFrozen := v.frozen.delta(key)
	active, hasActive := v.active.delta(key)
	if !hasFrozen && !hasActive {
		if !hasBase {
			return roar.Bitmap{}, false
		}
		return base.bitmap(), true
	}
	m.Reset()
	if hasBase {
		m.Or(base.bitmap())
	}
	if hasFrozen {
		m.Or(frozen.additions)
		m.AndNot(frozen.removals)
	}
	if hasActive {
		m.Or(active.additions)
		m.AndNot(active.removals)
	}
	return m.Freeze(), true
}

// effectiveTable folds the supplied deltas in order into a replacement
// posting table. Each layer merges its sorted keys with the table in one pass.
// Used by compaction, checkpointing, and hard-limit folds.
func (v *namespaceView) effectiveTable(deltas ...*namespaceDelta) postingTable {
	table := v.base
	folded := false
	for _, delta := range deltas {
		if delta == nil {
			continue
		}
		table = mergePostingDelta(table, delta)
		folded = true
	}
	if !folded {
		return table.clone()
	}
	return table
}

func mergePostingDelta(table postingTable, delta *namespaceDelta) postingTable {
	out := postingTable{
		keys:     make([]NamespaceKey, 0, len(table.keys)+len(delta.keys)),
		postings: make([]posting, 0, len(table.keys)+len(delta.keys)),
	}
	basePos, deltaPos := 0, 0
	for basePos < len(table.keys) || deltaPos < len(delta.keys) {
		if deltaPos == len(delta.keys) ||
			(basePos < len(table.keys) && compareNamespaceKey(table.keys[basePos], delta.keys[deltaPos]) < 0) {
			out.keys = append(out.keys, table.keys[basePos])
			out.postings = append(out.postings, table.postings[basePos])
			basePos++
			continue
		}
		key := delta.keys[deltaPos]
		bd := delta.values[key]
		if basePos == len(table.keys) || table.keys[basePos] != key {
			// Delta additions and removals are disjoint. With no base posting,
			// the additions are already the complete effective posting.
			if !bd.additions.IsEmpty() {
				out.keys = append(out.keys, key)
				out.postings = append(out.postings, makePosting(bd.additions))
			}
			deltaPos++
			continue
		}
		builder := roar.NewDeltaBuilderFrom(table.postings[basePos].bitmap())
		basePos++
		builder.ApplyDelta(bd.additions, bd.removals)
		if bit := builder.Freeze(); !bit.IsEmpty() {
			out.keys = append(out.keys, key)
			out.postings = append(out.postings, makePosting(bit))
		}
		deltaPos++
	}
	return out
}

// stagedChange is one prepared key-level change. base is the hash-level
// key from canonicalization; key is the dictionary-resolved key including
// any collision slot, filled under the writer lock. canonical borrows
// caller input through preparation and is copied only for new dictionary
// values. next chains changes sharing a base hash but differing in
// canonical bytes (hash collisions in one batch). add and remove hold
// unsorted caller IDs until compose/encode canonicalizes them.
type stagedChange struct {
	base      NamespaceKey
	key       NamespaceKey
	canonical []byte
	// next chains collision siblings sharing a base hash; -1 ends the
	// chain. An int avoids conversions on 64-bit platforms for a
	// prepare-only structure.
	next   int
	add    []schema.ImageID
	remove []schema.ImageID
}
