package bitmapidx

import (
	"slices"

	"github.com/evanoberholster/imageIdx/idset"
)

// bitmapDelta is an immutable set of recent corrections applied on top of the
// Base: additions are IDs to include, removals are IDs to exclude. The two
// sets are kept disjoint; if an ID is named in both, removal wins.
type bitmapDelta struct {
	additions idset.Ref
	removals  idset.Ref
}

// newBitmapDelta builds a delta from raw ID slices, canonicalizing the input
// so additions and removals are disjoint. It returns nil when the delta is
// empty, so a published view can simply hold nil for an absent layer.
func newBitmapDelta(additions, removals []ImageID) *bitmapDelta {
	if len(additions) == 0 && len(removals) == 0 {
		return nil
	}
	add := idset.New(additions)
	remove := idset.New(removals)
	// As with Apply, removal wins when an ID appears in both sets.
	if add.Valid() && remove.Valid() {
		builder := idset.Clone(add)
		builder.AndNot(remove)
		add = builder.Freeze()
	}
	if add.IsEmpty() && remove.IsEmpty() {
		return nil
	}
	return &bitmapDelta{additions: add, removals: remove}
}

// len reports the total number of corrected IDs in the delta. It is used only
// to drive rotation thresholds, never by the query planner.
func (d *bitmapDelta) len() int {
	if d == nil {
		return 0
	}
	return int(d.additions.Cardinality() + d.removals.Cardinality())
}

// state reports whether the delta names id, and if so the desired membership.
// The second return value distinguishes "not touched" from "explicitly
// removed", which callers use to decide whether to fall through to the layer
// below.
func (d *bitmapDelta) state(id ImageID) (present, touched bool) {
	if d == nil {
		return false, false
	}
	if d.additions.Contains(id) {
		return true, true
	}
	if d.removals.Contains(id) {
		return false, true
	}
	return false, false
}

// indexView is one immutable, atomically published snapshot of an Index.
// Everything reachable from a view is immutable for the view's lifetime, which
// is what makes reads lock-free.
//
// A view has at most three layers, with the newest taking precedence:
//
//	active  ->  frozen  ->  base
//
// cardinality stores the Base cardinality only; query planning deliberately
// ignores delta cardinalities.
type indexView struct {
	version   uint16
	indexType IndexType
	flags     uint32

	indexID uint64
	valueID uint64

	cardinality uint64
	generation  uint64

	base   idset.Ref
	frozen *bitmapDelta
	active *bitmapDelta
}

// contains reports whether id is in the effective set, walking the layers from
// newest to oldest. It allocates nothing.
func (v *indexView) contains(id ImageID) bool {
	if v == nil {
		return false
	}
	if present, ok := v.active.state(id); ok {
		return present
	}
	if present, ok := v.frozen.state(id); ok {
		return present
	}
	return v.base.Contains(id)
}

// hasDeltas reports whether the view carries any reader-visible correction
// layers. Delta-free views can be read directly from the Base without a clone.
func (v *indexView) hasDeltas() bool {
	return v != nil && (v.frozen != nil || v.active != nil)
}

// appendTo appends the effective IDs in ascending order without materializing
// a merged bitmap. Sufficient destination capacity makes both paths
// allocation-free.
func (v *indexView) appendTo(dst []ImageID) []ImageID {
	if v == nil {
		return dst
	}
	if !v.hasDeltas() {
		return v.base.AppendTo(dst)
	}
	start := len(dst)
	dst = v.base.AppendTo(dst)
	if v.frozen != nil {
		dst = v.frozen.additions.AppendTo(dst)
	}
	if v.active != nil {
		dst = v.active.additions.AppendTo(dst)
	}
	out := start
	for _, id := range dst[start:] {
		if v.contains(id) {
			dst[out] = id
			out++
		}
	}
	dst = dst[:out]
	values := dst[start:]
	slices.Sort(values)
	values = slices.Compact(values)
	return dst[:start+len(values)]
}

// effectiveBitmap materializes the fully merged set as a new immutable Ref.
// It clones the Base, so prefer contains or appendTo when a full set is not
// required.
func (v *indexView) effectiveBitmap() idset.Ref {
	if v == nil {
		return idset.Empty()
	}
	builder := v.effectiveBuilder()
	return builder.Freeze()
}

// effectiveBuilder clones the Base into a writer-owned Builder and applies the
// Frozen and Active corrections. The returned Builder must not escape without
// first being frozen.
func (v *indexView) effectiveBuilder() idset.Builder {
	if v == nil {
		return idset.Clone(idset.Empty())
	}
	builder := idset.Clone(v.base)
	applyDelta(&builder, v.frozen)
	applyDelta(&builder, v.active)
	return builder
}

// applyDelta applies a delta's additions and removals to a Builder in a
// single pass.
func applyDelta(dst *idset.Builder, delta *bitmapDelta) {
	if delta == nil {
		return
	}
	dst.ApplyDelta(delta.additions, delta.removals)
}
