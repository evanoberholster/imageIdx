package roar

import "slices"

// inlineDeltaCapacity is the number of pending additions and removals each
// DeltaBuilder stores without allocating. Most mutations are small; the
// buffers fall back to heap slices only when a batch exceeds this size.
const inlineDeltaCapacity = 64

// DeltaBuilder is a writer-owned builder that accumulates raw additions and
// removals against an immutable Bitmap and materializes the result on demand.
// It backs small, high-frequency corrections such as the delta layers of a
// copy-on-write index, where rebuilding an entire bitmap on every change
// would be wasteful.
//
// Unlike whole-set free functions (Or, And, AndNot), which materialize one
// result per call, DeltaBuilder defers raw ID additions and removals and
// folds them once at Freeze.
//
// Pending additions are applied before pending removals, so a removal wins
// when the same ID is both added and removed within one batch. This matches
// the "removal wins" convention used by bitmapidx mutation handling.
//
// Set operations (Or, And, AndNot) flush pending additions/removals first and
// then combine with the current bitmap, preserving sequential semantics.
//
// Whole-set combinations reuse an embedded result Builder. Final bitmaps are
// always freshly assembled so Freeze stays independent of later reuse.
//
// DeltaBuilder is not safe for concurrent use. Confine each DeltaBuilder to
// one goroutine. Do not copy a DeltaBuilder after first use: pending slices
// and Builder scratch may alias its own inline buffers.
//
// Selection rule: writers that publish immutable Bitmaps (index apply paths)
// use DeltaBuilder; single-goroutine query workspaces accumulating transient
// sets use MutableBitmap below, which recycles its assembly buffer across
// queries instead of guaranteeing Freeze independence.
type DeltaBuilder struct {
	bitmap    Bitmap
	adds      []uint64
	removes   []uint64
	addBuf    [inlineDeltaCapacity]uint64
	removeBuf [inlineDeltaCapacity]uint64
	fold      Builder
	dirty     bool
}

// NewDeltaBuilder returns an empty DeltaBuilder.
func NewDeltaBuilder() *DeltaBuilder {
	m := &DeltaBuilder{}
	m.adds = m.addBuf[:0]
	m.removes = m.removeBuf[:0]
	m.fold.scratchValues = m.fold.valuesBuf[:0]
	return m
}

// NewDeltaBuilderFrom returns a DeltaBuilder seeded with b. b is retained
// by reference and never mutated; the builder only replaces it.
func NewDeltaBuilderFrom(b Bitmap) *DeltaBuilder {
	m := &DeltaBuilder{bitmap: b}
	m.adds = m.addBuf[:0]
	m.removes = m.removeBuf[:0]
	m.fold.scratchValues = m.fold.valuesBuf[:0]
	return m
}

// AddMany stages additions of ids. They are merged into the bitmap lazily at
// the next set operation or Freeze.
func (m *DeltaBuilder) AddMany(ids []uint64) {
	if len(ids) == 0 {
		return
	}
	m.adds = append(m.adds, ids...)
	m.dirty = true
}

// Remove stages a removal of id. InvalidImageID is ignored.
func (m *DeltaBuilder) Remove(id uint64) {
	if id == InvalidImageID {
		return
	}
	m.removes = append(m.removes, id)
	m.dirty = true
}

// Or unions b into the builder's current bitmap.
func (m *DeltaBuilder) Or(b Bitmap) {
	m.foldSet(b, m.bitmap.KeyCount()+b.KeyCount(), m.bitmap.LenInBytes()+b.LenInBytes(), OrInto)
}

// And intersects b with the builder's current bitmap.
func (m *DeltaBuilder) And(b Bitmap) {
	m.foldSet(b, min(m.bitmap.KeyCount(), b.KeyCount()), min(m.bitmap.LenInBytes(), b.LenInBytes()), AndInto)
}

// AndNot subtracts b from the builder's current bitmap.
func (m *DeltaBuilder) AndNot(b Bitmap) {
	m.foldSet(b, m.bitmap.KeyCount(), m.bitmap.LenInBytes(), AndNotInto)
}

// ApplyDelta folds additions minus removals into the builder in a single
// pass, without materializing separate union/difference bitmaps. Pending
// additions/removals are flushed first, preserving sequential semantics.
func (m *DeltaBuilder) ApplyDelta(add, remove Bitmap) {
	m.flush()
	if add.IsEmpty() && remove.IsEmpty() {
		return
	}
	m.fold.prepareResult(m.bitmap.KeyCount()+add.KeyCount(), m.bitmap.LenInBytes()+add.LenInBytes())
	applyInto(&m.fold, m.bitmap, add, remove)
	m.bitmap = Bitmap{data: m.fold.finishResult(nil, false)}
}

// foldSet applies one whole-set operation with the shared result Builder. The
// result is freshly assembled, so a bitmap returned by Freeze stays
// independent of later reuse.
func (m *DeltaBuilder) foldSet(operand Bitmap, keyCount, dataBytes int, op func(dst *Builder, a, b Bitmap)) {
	m.flush()
	m.fold.prepareResult(keyCount, dataBytes)
	op(&m.fold, m.bitmap, operand)
	m.bitmap = Bitmap{data: m.fold.finishResult(nil, false)}
}

// Reset clears the builder while retaining all allocated capacity, so
// workspace-owned reuse across queries never regrows scratch.
func (m *DeltaBuilder) Reset() {
	m.bitmap = Bitmap{}
	m.adds = m.adds[:0]
	m.removes = m.removes[:0]
	m.fold.Reset()
	m.dirty = false
}

// Freeze materializes pending additions/removals and returns the immutable
// Bitmap. The builder remains usable afterward; the returned Bitmap is
// independent of it.
func (m *DeltaBuilder) Freeze() Bitmap {
	m.flush()
	return m.bitmap
}

// flush merges pending additions (then removals) into the current bitmap in a
// single pass, materializing no intermediate union bitmap.
func (m *DeltaBuilder) flush() {
	if !m.dirty {
		return
	}
	m.dirty = false
	add := bitmapFromUnique(m.adds)
	remove := bitmapFromUnique(m.removes)
	m.adds = m.adds[:0]
	m.removes = m.removes[:0]
	if add.IsEmpty() && remove.IsEmpty() {
		return
	}
	m.fold.prepareResult(m.bitmap.KeyCount()+add.KeyCount(), m.bitmap.LenInBytes()+add.LenInBytes())
	applyInto(&m.fold, m.bitmap, add, remove)
	m.bitmap = Bitmap{data: m.fold.finishResult(nil, false)}
}

// canonicalizeIDs sorts ids in place, removes duplicates and the reserved
// zero ID, and returns the remaining strictly increasing IDs.
func canonicalizeIDs(ids []uint64) []uint64 {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) != 0 && ids[0] == InvalidImageID {
		ids = ids[1:]
	}
	return ids
}

// bitmapFromUnique sorts ids in place, removes duplicates and the reserved
// zero ID, and returns a Bitmap over the remaining strictly increasing IDs.
func bitmapFromUnique(ids []uint64) Bitmap {
	ids = canonicalizeIDs(ids)
	if len(ids) == 0 {
		return Bitmap{}
	}
	bitmap, err := FromSorted(ids)
	if err != nil {
		// Unreachable: ids are sorted, unique, and free of zero.
		return Bitmap{}
	}
	return bitmap
}

// MutableBitmap is a transient, single-owner accumulation buffer for
// search-time set construction. Unlike an immutable Bitmap it is never
// published, persisted, or shared across goroutines: reads canonicalize
// lazily, scratch is reused across Reset calls, and no alignment,
// stored-cardinality, or validation invariants are maintained between
// operations.
//
// Raw IDs stage in a reusable buffer and canonicalize at most once per
// materialization; whole-set folds apply immediately into owned canonical
// state, so retained bytes stay valid after the source index's read guard is
// released. Fold assemblies recycle one owned result buffer, so repeated
// accumulation allocates only on growth. Materialization takes the sort-only
// path when no fold happened, freezing once otherwise.
//
// Internally the folded path delegates to an embedded DeltaBuilder, so no
// set kernel is forked here: MutableBitmap only adds the transient contract
// (reuse plus materialize conveniences) that writer paths must not rely on.
// Writer-owned construction keeps using DeltaBuilder directly.
//
// A MutableBitmap is not safe for concurrent use. Confine each instance to
// one goroutine, typically owned by a query workspace and reset per query.
// Do not copy a MutableBitmap after first use.
type MutableBitmap struct {
	delta  DeltaBuilder
	folded bool
	// result is the owned assembly buffer recycled across folds. It always
	// holds the latest assembly (or nothing before the first fold), so
	// overwriting it during assembly never reads live state: merges complete
	// before assembly starts. Reset retains it for the next query.
	result []byte
}

// NewMutableBitmap returns an empty MutableBitmap with inline scratch wired,
// so small accumulations allocate nothing before first growth.
func NewMutableBitmap() *MutableBitmap {
	m := &MutableBitmap{}
	m.delta = *NewDeltaBuilder()
	return m
}

// Reset clears the accumulation while retaining all scratch capacity,
// including the canonical result buffer once materialization has grown it.
func (m *MutableBitmap) Reset() {
	m.delta.Reset()
	m.folded = false
}

// AddMany stages raw additions. IDs may arrive in any order and may repeat;
// the reserved zero ID is dropped at materialization.
func (m *MutableBitmap) AddMany(ids []uint64) { m.delta.AddMany(ids) }

// Remove stages a removal of id. InvalidImageID is ignored. Any removal
// forces the folded materialization path.
func (m *MutableBitmap) Remove(id uint64) {
	m.delta.Remove(id)
	m.folded = true
}

// foldInto flushes staged IDs, combines the accumulation with operand, and
// assembles the result into the recycled buffer.
func (m *MutableBitmap) foldInto(operand Bitmap, keyCount, dataBytes int, op func(dst *Builder, a, b Bitmap)) {
	d := &m.delta
	m.flushPending()
	d.fold.prepareResult(keyCount, dataBytes)
	op(&d.fold, d.bitmap, operand)
	m.assemble()
	m.folded = true
}

// Or folds the immutable bitmap b into the accumulation.
func (m *MutableBitmap) Or(b Bitmap) {
	d := &m.delta
	m.foldInto(b, d.bitmap.KeyCount()+b.KeyCount(), d.bitmap.LenInBytes()+b.LenInBytes(), OrInto)
}

// And intersects the accumulation with the immutable bitmap b.
func (m *MutableBitmap) And(b Bitmap) {
	d := &m.delta
	m.foldInto(b, min(d.bitmap.KeyCount(), b.KeyCount()), min(d.bitmap.LenInBytes(), b.LenInBytes()), AndInto)
}

// AndNot subtracts the immutable bitmap b from the accumulation.
func (m *MutableBitmap) AndNot(b Bitmap) {
	d := &m.delta
	m.foldInto(b, d.bitmap.KeyCount(), d.bitmap.LenInBytes(), AndNotInto)
}

// ApplyDelta folds additions minus removals into the accumulation in a
// single pass.
func (m *MutableBitmap) ApplyDelta(add, remove Bitmap) {
	d := &m.delta
	m.flushPending()
	if add.IsEmpty() && remove.IsEmpty() {
		return
	}
	d.fold.prepareResult(d.bitmap.KeyCount()+add.KeyCount(), d.bitmap.LenInBytes()+add.LenInBytes())
	applyInto(&d.fold, d.bitmap, add, remove)
	m.assemble()
	m.folded = true
}

// Freeze materializes the accumulation as a Bitmap over the mutable's live
// scratch: further accumulation may recycle its bytes. Clone the result to
// retain it across mutation. When only reads follow, prefer AppendTo or
// Cardinality, which never retain.
func (m *MutableBitmap) Freeze() Bitmap {
	m.flushPending()
	return m.delta.bitmap
}

// assemble folds the pending builder result into canonical state, recycling
// the owned result buffer. Merge sources are read before assembly writes,
// so overwriting the previous assembly is sound.
func (m *MutableBitmap) assemble() {
	d := &m.delta
	d.bitmap = Bitmap{data: d.fold.finishResult(m.result, true)}
	m.result = d.bitmap.Bytes()
}

// flushPending merges staged additions (then removals) into canonical state,
// recycling the result buffer. It mirrors DeltaBuilder.flush, which writer
// paths keep using unchanged.
func (m *MutableBitmap) flushPending() {
	d := &m.delta
	if !d.dirty {
		return
	}
	d.dirty = false
	add := bitmapFromUnique(d.adds)
	remove := bitmapFromUnique(d.removes)
	d.adds = d.adds[:0]
	d.removes = d.removes[:0]
	if add.IsEmpty() && remove.IsEmpty() {
		return
	}
	d.fold.prepareResult(d.bitmap.KeyCount()+add.KeyCount(), d.bitmap.LenInBytes()+add.LenInBytes())
	applyInto(&d.fold, d.bitmap, add, remove)
	m.assemble()
}

// AppendTo appends the accumulated IDs in ascending order to dst. ID-only
// accumulation canonicalizes the staged IDs in place without touching bitmap
// machinery; otherwise it freezes once. Prefer it over Freeze plus AppendTo
// when the immutable result need not be retained.
func (m *MutableBitmap) AppendTo(dst []uint64) []uint64 {
	if m.needsFreeze() {
		return m.delta.Freeze().AppendTo(dst)
	}
	m.delta.adds = canonicalizeIDs(m.delta.adds)
	return append(dst, m.delta.adds...)
}

// Cardinality returns the number of accumulated IDs. Like AppendTo it takes
// the sort-only path when no fold happened.
func (m *MutableBitmap) Cardinality() uint64 {
	if m.needsFreeze() {
		return m.delta.Freeze().Cardinality()
	}
	m.delta.adds = canonicalizeIDs(m.delta.adds)
	return uint64(len(m.delta.adds))
}

// needsFreeze reports whether materialization must go through the embedded
// builder: any fold or staged removal, or a retained canonical bitmap from
// an earlier Freeze, rules out the sort-only path.
func (m *MutableBitmap) needsFreeze() bool {
	return m.folded || !m.delta.bitmap.IsEmpty() || len(m.delta.removes) != 0
}
