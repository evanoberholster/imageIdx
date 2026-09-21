package bitmapidx

import (
	"slices"

	"github.com/evanoberholster/imageIdx/roar"
)

// postingTable is the immutable base of one field: keys sorted by
// FieldKey and their postings at matching positions. Slices are shared
// across publications and never mutated after publication; writers clone
// before folding deltas.
type postingTable struct {
	keys     []FieldKey
	postings []posting
}

func (t *postingTable) get(key FieldKey) (*posting, bool) {
	index, ok := slices.BinarySearchFunc(t.keys, key, func(e FieldKey, k FieldKey) int {
		return compareFieldKey(e, k)
	})
	if !ok {
		return nil, false
	}
	return &t.postings[index], true
}

func (t *postingTable) clone() postingTable {
	return postingTable{keys: slices.Clone(t.keys), postings: slices.Clone(t.postings)}
}

func (t *postingTable) upsert(key FieldKey, p posting) {
	index, ok := slices.BinarySearchFunc(t.keys, key, func(e FieldKey, k FieldKey) int {
		return compareFieldKey(e, k)
	})
	if ok {
		t.postings[index] = p
		return
	}
	t.keys = slices.Insert(t.keys, index, key)
	t.postings = slices.Insert(t.postings, index, p)
}

func (t *postingTable) remove(key FieldKey) {
	index, ok := slices.BinarySearchFunc(t.keys, key, func(e FieldKey, k FieldKey) int {
		return compareFieldKey(e, k)
	})
	if !ok {
		return
	}
	t.keys = slices.Delete(t.keys, index, index+1)
	t.postings = slices.Delete(t.postings, index, index+1)
}

// bitmapDelta is one immutable correction for a single field value:
// additions minus removals, already canonicalized so the sets are
// disjoint. Values are roaring bitmaps shared across views.
type bitmapDelta struct {
	additions roar.Bitmap
	removals  roar.Bitmap
}

// newBitmapDelta canonicalizes additions minus removals into an immutable
// correction. It returns nil when the correction is empty, so callers can
// keep delta maps sparse.
func newBitmapDelta(additions, removals []ImageID) *bitmapDelta {
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

// fieldDelta is one immutable correction layer: per-key added/removed
// IDs. values holds corrections by resolved key, keys is the sorted key
// set for ordered scans, and ids is the total corrected ID count used for
// rotation decisions. Untouched entries are shared across publications.
type fieldDelta struct {
	values map[FieldKey]*bitmapDelta
	keys   []FieldKey
	ids    int
}

func (d *fieldDelta) delta(key FieldKey) (*bitmapDelta, bool) {
	if d == nil {
		return nil, false
	}
	bd, ok := d.values[key]
	return bd, ok
}

// state reports the layer's correction for (key, id), if any. Additions
// win within the layer because newBitmapDelta canonicalizes remove-wins
// before storage.
func (d *fieldDelta) state(key FieldKey, id ImageID) (present, touched bool) {
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

// fieldView is one immutable, atomically published snapshot of a field.
// Lookup precedence is active, then frozen, then base. dictGen mirrors
// the dictionary generation so compiled queries can fast-path when the
// value set is unchanged; generation orders view publications.
type fieldView struct {
	field      Field
	base       postingTable
	frozen     *fieldDelta
	active     *fieldDelta
	dict       *dictionaryView
	dictGen    uint64
	generation uint64
}

func (v *fieldView) hasDeltas() bool { return v.frozen != nil || v.active != nil }

func (v *fieldView) contains(key FieldKey, id ImageID) bool {
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

func (v *fieldView) containsWithoutActive(key FieldKey, id ImageID) bool {
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
func (v *fieldView) effective(key FieldKey) (*posting, bool) {
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

// effectiveTable folds the supplied deltas into a replacement posting
// table. It clones Base once, then upserts or removes per touched key.
// Used by compaction, checkpointing, and hard-limit folds.
func (v *fieldView) effectiveTable(deltas ...*fieldDelta) postingTable {
	table := v.base.clone()
	for _, delta := range deltas {
		if delta == nil {
			continue
		}
		for key, bd := range delta.values {
			base, hasBase := table.get(key)
			var builder *roar.DeltaBuilder
			if hasBase {
				builder = roar.NewDeltaBuilderFrom(base.bitmap())
			} else {
				builder = roar.NewDeltaBuilder()
			}
			builder.ApplyDelta(bd.additions, bd.removals)
			out := builder.Freeze()
			if out.IsEmpty() {
				table.remove(key)
			} else {
				table.upsert(key, makePosting(out))
			}
		}
	}
	return table
}

// stagedChange is one prepared key-level change. base is the hash-level
// key from canonicalization; key is the dictionary-resolved key including
// any collision slot, filled under the writer lock. canonical borrows
// caller input through preparation and is copied only for new dictionary
// values. next chains changes sharing a base hash but differing in
// canonical bytes (hash collisions in one batch). add and remove hold
// unsorted caller IDs until compose/encode canonicalizes them.
type stagedChange struct {
	base      FieldKey
	key       FieldKey
	canonical []byte
	// next chains collision siblings sharing a base hash; -1 ends the
	// chain. An int avoids conversions on 64-bit platforms for a
	// prepare-only structure.
	next   int
	add    []ImageID
	remove []ImageID
}
