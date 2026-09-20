package bitmapidx

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
	"github.com/evanoberholster/imageIdx/roar"
)

var (
	// ErrInvalidMutation reports a malformed membership transition.
	ErrInvalidMutation = errors.New("bitmapidx: invalid mutation")
	// ErrInvalidImageID reports ImageID zero, which is reserved.
	ErrInvalidImageID = errors.New("bitmapidx: image ID zero is reserved")
	// ErrDuplicateImageID reports an image already holding a value in a field
	// that allows only one value per image.
	ErrDuplicateImageID = errors.New("bitmapidx: duplicate image ID")
)

// FieldConfig controls field delta rotation and compaction. The zero
// value is valid; NewFieldIndex fills defaults. ActiveTarget rotates
// Active to Frozen and schedules background compaction; HardDeltaLimit
// and MaxDeltaValues instead fold the batch directly into Base on the
// write path, bounding every reader-visible delta.
type FieldConfig struct {
	// ActiveTarget is the Active ID count at which a write rotates the
	// delta to Frozen and schedules background compaction.
	ActiveTarget int
	// HardDeltaLimit bounds the IDs in one visible delta. Exceeding it
	// folds the batch into Base synchronously.
	HardDeltaLimit int
	// MaxDeltaValues bounds the distinct values touched in one visible
	// delta. Exceeding it folds the batch into Base synchronously.
	MaxDeltaValues int
	// CompactDelay pauses background compaction, primarily for tests and
	// workload tuning. Zero compacts immediately.
	CompactDelay time.Duration
}

const (
	// DefaultFieldActiveTarget is the default Active-to-Frozen rotation bound.
	DefaultFieldActiveTarget = 2_048
	// DefaultFieldHardDeltaLimit is the maximum default visible delta size.
	DefaultFieldHardDeltaLimit = 4_096
	// DefaultFieldMaxDeltaValues is the default distinct-value delta bound.
	DefaultFieldMaxDeltaValues = 256
)

func (c FieldConfig) withDefaults() (FieldConfig, error) {
	if c.ActiveTarget < 0 || c.HardDeltaLimit < 0 || c.MaxDeltaValues < 0 || c.CompactDelay < 0 {
		return FieldConfig{}, ErrInvalidConfig
	}
	if c.ActiveTarget == 0 {
		c.ActiveTarget = DefaultFieldActiveTarget
	}
	if c.HardDeltaLimit == 0 {
		c.HardDeltaLimit = DefaultFieldHardDeltaLimit
	}
	if c.MaxDeltaValues == 0 {
		c.MaxDeltaValues = DefaultFieldMaxDeltaValues
	}
	if c.HardDeltaLimit < 2 || c.ActiveTarget >= c.HardDeltaLimit {
		return FieldConfig{}, ErrInvalidConfig
	}
	return c, nil
}

// FieldMetadata is a copied summary of the current field snapshot:
// the immutable definition, the monotonically increasing view
// generation, and the number of values in Base.
type FieldMetadata struct {
	Field      Field
	Generation uint64
	BaseValues uint64
}

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

// FieldIndex is one immutable-field, multi-value index. Readers load one
// atomically published fieldView and take no locks beyond the reader
// tracker; writers serialize through writerMu. One batch publishes
// atomically across postings and dictionary.
//
// Reads against a closed index report empty results, as do queries
// compiled after close; writes and maintenance report ErrClosed. The zero
// value is not usable; construct with NewFieldIndex. Store-owned indexes
// (owner != nil) route writes through the store WAL and must be closed via
// the store.
type FieldIndex struct {
	owner    *FieldStore // immutable before exposing the handle
	view     atomic.Pointer[fieldView]
	readers  *indexutil.ReaderTracker
	writerMu sync.Mutex
	config   FieldConfig
	closed   atomic.Bool
	workers  sync.WaitGroup
	pending  atomic.Int64
	dict     dictionaryWriter
	analyzer *textanalyzer.Analyzer
}

// NewFieldIndex constructs an empty field index from a validated
// definition. Config zero values take defaults. The definition is
// deep-copied; text fields also build their analyzer here so writes and
// queries never construct one per batch.
func NewFieldIndex(field Field, config FieldConfig) (*FieldIndex, error) {
	if err := validateField(field); err != nil {
		return nil, err
	}
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	field.Normalizer = field.Normalizer.clone()
	field.Analyzer = field.Analyzer.clone()
	index := &FieldIndex{config: config, readers: &indexutil.ReaderTracker{}}
	if field.Kind == KindText {
		index.analyzer, err = textanalyzer.New(field.Analyzer.Code, field.Analyzer.Version, field.Analyzer.Params)
		if err != nil {
			return nil, err
		}
	}
	index.view.Store(&fieldView{field: field, dict: index.dict.view(), generation: 1})
	return index, nil
}

// Field returns a deep copy of the immutable field definition.
func (i *FieldIndex) Field() Field { return i.view.Load().field.clone() }

// Metadata returns a copied summary of the current snapshot.
func (i *FieldIndex) Metadata() FieldMetadata {
	view := i.view.Load()
	return FieldMetadata{Field: view.field.clone(), Generation: view.generation, BaseValues: uint64(len(view.base.keys))}
}

// PendingCompactions reports the number of in-flight background
// compactions folding Frozen into Base.
func (i *FieldIndex) PendingCompactions() int64 { return i.pending.Load() }

// WaitIdle blocks until background compactions drain or ctx is cancelled.
// It polls every 100µs via indexutil and never takes the writer lock.
func (i *FieldIndex) WaitIdle(ctx context.Context) error {
	return indexutil.WaitForCondition(ctx, func() bool { return i.pending.Load() == 0 })
}

// Contains reports whether id holds value in this field. Unknown values
// report false with no error; kind mismatches and oversized values report
// errors. Closed indexes report false.
func (i *FieldIndex) Contains(id ImageID, value Value) (bool, error) {
	base, canonical, err := i.keyForValue(i.Field(), value)
	if err != nil {
		return false, err
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return false, nil
	}
	key := base
	if canonical != nil {
		resolved, ok := view.dict.resolve(base, canonical)
		if !ok {
			i.readers.Release(token)
			return false, nil
		}
		key = resolved
	}
	present := view.contains(key, id)
	i.readers.Release(token)
	return present, nil
}

// ContainsKey reports whether id holds the posting named by key,
// including its collision slot. It skips canonicalization for callers
// that already resolved keys.
func (i *FieldIndex) ContainsKey(id ImageID, key FieldKey) bool {
	token := i.readers.Acquire()
	view := i.view.Load()
	present := !i.closed.Load() && view.contains(key, id)
	i.readers.Release(token)
	return present
}

// Add applies membership additions atomically. Already-present memberships
// are no-ops. Text fields reject Add; use ReplaceText.
func (i *FieldIndex) Add(entries ...Entry) error {
	if i.Field().Kind == KindText {
		return ErrUnsupportedOperation
	}
	return i.applyPrepared(FieldMutation{Add: entries})
}

// Remove applies membership removals atomically. Unknown removals are
// no-ops and never create dictionary entries. Text fields reject Remove;
// use ReplaceText.
func (i *FieldIndex) Remove(entries ...Entry) error {
	if i.Field().Kind == KindText {
		return ErrUnsupportedOperation
	}
	return i.applyPrepared(FieldMutation{Remove: entries})
}

// Apply atomically applies one membership batch with remove-wins
// semantics for the same (key, ImageID). Text fields reject Apply; use
// ReplaceText.
func (i *FieldIndex) Apply(mutation FieldMutation) error {
	if i.Field().Kind == KindText {
		return ErrUnsupportedOperation
	}
	return i.applyPrepared(mutation)
}

// Move applies explicit value transitions for continuous and geo fields.
// It rejects duplicate images in one batch, no-op moves, and stale From
// values (via validateGeoChanges for geo). Geo may omit presence flags
// because a level-16 key is never zero; continuous keeps explicit flags
// because zero is a valid grid value.
func (i *FieldIndex) Move(moves ...Move) error {
	field := i.Field()
	if field.Kind != KindContinuous && field.Kind != KindGeo {
		return ErrFieldMismatch
	}
	value := func(v int64) Value {
		if field.Kind == KindGeo {
			return GeoValue(uint64(v))
		}
		return IntValue(v)
	}
	var mutation FieldMutation
	seen := make(map[ImageID]struct{}, len(moves))
	for _, move := range moves {
		if move.ImageID == 0 {
			return ErrInvalidImageID
		}
		if _, ok := seen[move.ImageID]; ok {
			return ErrInvalidMutation
		}
		seen[move.ImageID] = struct{}{}
		if field.Kind == KindGeo {
			if !move.HasFrom && move.From != 0 {
				move.HasFrom = true
			}
			if !move.HasTo && move.To != 0 {
				move.HasTo = true
			}
		}
		if !move.HasFrom && !move.HasTo {
			continue
		}
		if move.HasFrom && move.HasTo && move.From == move.To {
			continue
		}
		if move.HasFrom {
			mutation.Remove = append(mutation.Remove, Entry{ImageID: move.ImageID, Value: value(move.From)})
		}
		if move.HasTo {
			mutation.Add = append(mutation.Add, Entry{ImageID: move.ImageID, Value: value(move.To)})
		}
	}
	return i.applyPrepared(mutation)
}

// ReplaceText replaces complete logical text fields by diffing old and
// new analyzer token sets. Unchanged tokens produce no membership churn.
// Store-owned batches pre-check source bytes against a conservative
// fanout bound; the WAL encoder enforces the exact membership limit.
func (i *FieldIndex) ReplaceText(changes ...TextReplacement) error {
	field := i.Field()
	if field.Kind != KindText {
		return ErrFieldMismatch
	}
	if i.owner != nil {
		remaining := MaxFieldBatchBytes
		if len(changes) > MaxFieldBatchUpdates*batchFanoutBound {
			return ErrBatchTooLarge
		}
		for _, change := range changes {
			for _, docs := range [][]string{change.Before, change.After} {
				for _, doc := range docs {
					if len(doc) > remaining {
						return ErrBatchTooLarge
					}
					remaining -= len(doc)
				}
			}
		}
	}
	var mutation FieldMutation
	for _, change := range changes {
		if change.ImageID == 0 {
			return ErrInvalidImageID
		}
		before, err := analyzeSet(i.analyzer, change.Before)
		if err != nil {
			return err
		}
		after, err := analyzeSet(i.analyzer, change.After)
		if err != nil {
			return err
		}
		for token := range before {
			if _, ok := after[token]; ok {
				continue
			}
			mutation.Remove = append(mutation.Remove, Entry{ImageID: change.ImageID, Value: Value{Kind: KindText, Bytes: []byte(token)}})
		}
		for token := range after {
			if _, ok := before[token]; ok {
				continue
			}
			mutation.Add = append(mutation.Add, Entry{ImageID: change.ImageID, Value: Value{Kind: KindText, Bytes: []byte(token)}})
		}
	}
	return i.applyPrepared(mutation)
}

// Compact synchronously merges Frozen and Active into a new Base and
// seals the dictionary. It preserves the visible value set, so only
// posting storage moves.
func (i *FieldIndex) Compact() error {
	if i.owner != nil {
		i.owner.writeMu.Lock()
		defer i.owner.writeMu.Unlock()
		if err := i.owner.writable(); err != nil {
			return err
		}
	}
	i.writerMu.Lock()
	if i.closed.Load() {
		i.writerMu.Unlock()
		return ErrClosed
	}
	current := i.view.Load()
	base := current.effectiveTable(current.frozen, current.active)
	i.dict.seal()
	i.publishLocked(current, base, nil, nil)
	i.writerMu.Unlock()
	i.readers.WaitForPriorReaders()
	return nil
}

// Vacuum compacts the field and drops dictionary values with no
// remaining posting. It is the only operation that removes dead
// dictionary entries, and the only one that advances the dictionary
// generation without new values.
func (i *FieldIndex) Vacuum() error {
	if i.owner != nil {
		i.owner.writeMu.Lock()
		defer i.owner.writeMu.Unlock()
		if err := i.owner.writable(); err != nil {
			return err
		}
	}
	i.writerMu.Lock()
	if i.closed.Load() {
		i.writerMu.Unlock()
		return ErrClosed
	}
	current := i.view.Load()
	base := current.effectiveTable(current.frozen, current.active)
	i.rebuildDictionary(current, base)
	i.publishLocked(current, base, nil, nil)
	i.writerMu.Unlock()
	i.readers.WaitForPriorReaders()
	return nil
}

// Close marks a standalone field closed, waits for compactions and prior
// readers, and rejects further writes. Store-owned indexes reject Close;
// close the store instead.
func (i *FieldIndex) Close() error {
	if i.owner != nil {
		return ErrOwnedIndex
	}
	return i.closeInternal()
}

func (i *FieldIndex) closeInternal() error {
	i.writerMu.Lock()
	i.closed.Store(true)
	i.writerMu.Unlock()
	i.workers.Wait()
	i.readers.WaitForPriorReaders()
	return nil
}

// LookupValue appends the canonical bytes for key to dst. It reports
// hash-plus-slot presence without byte verification; use KeyForValue to
// resolve a value to its verified slot first.
func (i *FieldIndex) LookupValue(dst []byte, key FieldKey) ([]byte, bool) {
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return dst, false
	}
	out, ok := view.dict.lookup(dst, key)
	i.readers.Release(token)
	return out, ok
}

// Iterate visits one dictionary value per key in key order. Returning
// false stops early. Values alias chunk/mmap storage valid only for the
// call. It materializes merged state, so it allocates.
func (i *FieldIndex) Iterate(fn func(key FieldKey, value []byte) bool) {
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() {
		view.dict.iterate(fn)
	}
	i.readers.Release(token)
}

// Prefix visits dictionary values starting with prefix, in byte order of
// canonical values. Values alias storage valid only for the call. It
// re-sorts by value, so it is O(N log N); see dictionaryView.prefix.
func (i *FieldIndex) Prefix(prefix []byte, fn func(key FieldKey, value []byte) bool) {
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() {
		view.dict.prefix(prefix, fn)
	}
	i.readers.Release(token)
}

// KeyForValue canonicalizes and hashes value, returning its dictionary
// key including its collision slot. Absent values, and any value on a
// closed index, report ok=false. Continuous values have no dictionary
// entry and report their order key directly.
func (i *FieldIndex) KeyForValue(value Value) (FieldKey, bool, error) {
	field := i.Field()
	key, canonical, err := i.keyForValue(field, value)
	if err != nil {
		return FieldKey{}, false, err
	}
	if canonical == nil {
		return key, true, nil // continuous values have no dictionary entry
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return FieldKey{}, false, nil
	}
	resolved, ok := view.dict.resolve(key, canonical)
	i.readers.Release(token)
	if !ok {
		return key, false, nil
	}
	return resolved, true, nil
}

func (i *FieldIndex) applyPrepared(mutation FieldMutation) error {
	if i.closed.Load() {
		return ErrClosed
	}
	changes, err := i.prepare(i.Field(), mutation)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	return i.applyChanges(changes)
}

// prepare canonicalizes values, computes hash-level keys, and groups
// changes by (base hash, canonical bytes). It runs before the writer lock
// so analyzer and hashing work stays concurrent. Returned canonical
// slices borrow caller input and are copied under the lock only for
// genuinely new dictionary values. Owner pre-checks are conservative:
// entry counts scale by batchFanoutBound and byte budgets cap source
// bytes; the WAL encoder enforces exact membership limits later.
func (i *FieldIndex) prepare(field Field, mutation FieldMutation) ([]stagedChange, error) {
	if len(mutation.Add) == 0 && len(mutation.Remove) == 0 {
		return nil, nil
	}
	if i.owner != nil {
		if len(mutation.Add) > MaxFieldBatchUpdates*batchFanoutBound || len(mutation.Remove) > MaxFieldBatchUpdates*batchFanoutBound-len(mutation.Add) {
			return nil, ErrBatchTooLarge
		}
		remaining := MaxFieldBatchBytes
		for _, entries := range [][]Entry{mutation.Add, mutation.Remove} {
			for _, entry := range entries {
				if len(entry.Value.Bytes) > remaining {
					return nil, ErrBatchTooLarge
				}
				remaining -= len(entry.Value.Bytes)
			}
		}
	}
	position := make(map[FieldKey]int, len(mutation.Add)+len(mutation.Remove))
	changes := make([]stagedChange, 0, len(mutation.Add)+len(mutation.Remove))
	stage := func(entry Entry, add bool) error {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		key, canonical, err := i.keyForValue(field, entry.Value)
		if err != nil {
			return err
		}
		index, ok := position[key]
		if !ok {
			changes = append(changes, stagedChange{base: key, canonical: canonical, next: -1})
			index = len(changes) - 1
			position[key] = index
		} else {
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
func (i *FieldIndex) keyForValue(field Field, value Value) (FieldKey, []byte, error) {
	switch field.Kind {
	case KindEnum:
		if value.Kind != KindEnum {
			return FieldKey{}, nil, ErrValueKind
		}
		canonical, err := normalizeEnum(field.Normalizer, value.Bytes)
		if err != nil {
			return FieldKey{}, nil, err
		}
		if len(canonical) > MaxValueBytes {
			return FieldKey{}, nil, ErrValueTooLong
		}
		if canonical == nil {
			canonical = []byte{}
		}
		return hashCanonical(field.Type, KindEnum, canonical), canonical, nil
	case KindBool:
		if value.Kind != KindBool {
			return FieldKey{}, nil, ErrValueKind
		}
		canonical := []byte{'0'}
		if value.Bool {
			canonical[0] = '1'
		}
		return hashCanonical(field.Type, KindBool, canonical), canonical, nil
	case KindText:
		if value.Kind != KindText {
			return FieldKey{}, nil, ErrValueKind
		}
		if len(value.Bytes) > MaxValueBytes {
			return FieldKey{}, nil, ErrValueTooLong
		}
		return hashCanonical(field.Type, KindText, value.Bytes), value.Bytes, nil
	case KindContinuous:
		if value.Kind != KindContinuous {
			return FieldKey{}, nil, ErrValueKind
		}
		return continuousKey(field.Type, value.Int), nil, nil
	case KindGeo:
		if value.Kind != KindGeo {
			return FieldKey{}, nil, ErrValueKind
		}
		if value.Int < 0 {
			return FieldKey{}, nil, ErrInvalidS2Cell
		}
		key := uint64(value.Int)
		if _, err := CellIDFromKey(key); err != nil {
			return FieldKey{}, nil, err
		}
		return FieldKey{Hash: key, Type: field.Type}, nil, nil
	default:
		return FieldKey{}, nil, ErrInvalidField
	}
}

// applyChanges composes and publishes one prepared batch on the live
// path, with geo validation. See applyChangesMode for the ordering.
func (i *FieldIndex) applyChanges(changes []stagedChange) error {
	return i.applyChangesMode(changes, false)
}

// applyChangesReplay republishes WAL-decoded changes without live geo
// preconditions: replay uses idempotent set semantics because the WAL
// already validated the batch once.
func (i *FieldIndex) applyChangesReplay(changes []stagedChange) error {
	return i.applyChangesMode(changes, true)
}

// applyChangesMode serializes dictionary resolution, delta composition,
// WAL append (if owned), and view publication. It finishes every fallible
// step before the WAL append; the append is followed by exactly one
// non-fallible publication. On failure the dictionary writer rolls back to
// a chunk-cloned snapshot and aborts staged entries, so no phantom value
// survives. Rotation policy: hard limits fold into Base synchronously,
// ActiveTarget rotates Active to Frozen and schedules background
// compaction.
func (i *FieldIndex) applyChangesMode(changes []stagedChange, replay bool) error {
	if i.owner != nil {
		i.owner.writeMu.Lock()
		defer i.owner.writeMu.Unlock()
		if err := i.owner.writable(); err != nil {
			return err
		}
	}
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
	resolved, err := i.resolveChanges(current, changes)
	if err != nil {
		i.dict.abort()
		return err
	}
	if !replay {
		if err := validateGeoChanges(current, resolved); err != nil {
			return err
		}
	}
	var walPayload []byte
	var walUpdates uint32
	if i.owner != nil {
		// Bound and freeze the canonical frame before allocating replacement
		// postings. Encoding also removes duplicate/conflicting memberships.
		walPayload, walUpdates, err = encodeFieldChanges(resolved)
		if err != nil {
			return err
		}
	}
	active := composeFieldDelta(current, resolved)
	if active == nil && current.active == nil {
		return nil // every requested change already matched published state
	}

	base, frozen := current.base, current.frozen
	rotate := false
	if active != nil && (active.ids >= i.config.HardDeltaLimit || len(active.values) > i.config.MaxDeltaValues) {
		base = current.effectiveTable(current.frozen, active)
		frozen, active = nil, nil
	} else if frozen == nil && active != nil && active.ids >= i.config.ActiveTarget {
		frozen, active, rotate = active, nil, true
	}
	i.dict.seal()
	// Finish every fallible preparation before WAL append; the successful
	// append is followed by exactly one non-fallible root publication.
	next := i.nextView(current, base, frozen, active)
	if i.owner != nil {
		if err := i.owner.append(i, walPayload, uint32(len(resolved)), walUpdates); err != nil {
			return err
		}
	}
	i.view.Store(next)
	committed = true
	if rotate {
		i.scheduleCompaction(frozen)
	}
	return nil
}

// resolveChanges maps prepared values to dictionary keys with slots.
// Additions claim a collision slot and stage bytes; removal-only changes
// with no matching value are dropped as no-ops. Continuous and geo
// changes (nil canonical) pass through with their base key.
func (i *FieldIndex) resolveChanges(view *fieldView, changes []stagedChange) ([]stagedChange, error) {
	out := changes[:0]
	for index := range changes {
		change := changes[index]
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

// composeFieldDelta resolves the batch against the current view. It
// shallow-copies Active (untouched deltas stay shared) and recomputes
// only touched keys. IDs already named in Active keep their staged state
// without rechecking lower layers; only genuinely new IDs probe
// containsWithoutActive. Remove wins within the batch. It returns nil when
// the batch matches published state.
func composeFieldDelta(view *fieldView, changes []stagedChange) *fieldDelta {
	var values map[FieldKey]*bitmapDelta
	var keys []FieldKey
	ids := 0
	if view.active != nil {
		values = make(map[FieldKey]*bitmapDelta, len(view.active.values)+len(changes))
		for key, bd := range view.active.values {
			values[key] = bd
		}
		keys = slices.Clone(view.active.keys)
		ids = view.active.ids
	} else {
		values = make(map[FieldKey]*bitmapDelta, len(changes))
	}

	for _, change := range changes {
		adds := dedupeSortedIDs(change.add)
		rems := dedupeSortedIDs(change.remove)
		adds = subtractSortedIDs(adds, rems) // remove wins within the batch

		old, _ := view.active.delta(change.key)
		var oldAdds, oldRems []ImageID
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
		var newAdds, newRems []ImageID
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
				if index, ok := slices.BinarySearchFunc(keys, change.key, compareFieldKey); ok {
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
			index, _ := slices.BinarySearchFunc(keys, change.key, compareFieldKey)
			keys = slices.Insert(keys, index, change.key)
		}
		ids += len(addFinal) + len(remFinal) - oldLen
	}
	if len(values) == 0 {
		return nil
	}
	return &fieldDelta{values: values, keys: keys, ids: ids}
}

// dedupeSortedIDs sorts ids in place and returns the unique prefix. The
// input must be caller-owned; encode and compose both rely on sorted,
// deduplicated columns.
func dedupeSortedIDs(ids []ImageID) []ImageID {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// containsSortedID binary-searches an ascending slice.
func containsSortedID(ids []ImageID, id ImageID) bool {
	_, ok := slices.BinarySearch(ids, id)
	return ok
}

// subtractSortedIDs returns values minus minus, both ascending and
// unique. It reuses values backing when possible; callers must own values.
func subtractSortedIDs(values, minus []ImageID) []ImageID {
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
func mergeSortedIDs(first, second []ImageID) []ImageID {
	if len(first) == 0 {
		return second
	}
	if len(second) == 0 {
		return first
	}
	out := make([]ImageID, 0, len(first)+len(second))
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
func (i *FieldIndex) publishLocked(old *fieldView, base postingTable, frozen, active *fieldDelta) *fieldView {
	next := i.nextView(old, base, frozen, active)
	i.view.Store(next)
	return next
}

func (i *FieldIndex) nextView(old *fieldView, base postingTable, frozen, active *fieldDelta) *fieldView {
	return &fieldView{
		field:      old.field,
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
func (i *FieldIndex) scheduleCompaction(frozen *fieldDelta) {
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
func (i *FieldIndex) rebuildDictionary(view *fieldView, base postingTable) {
	live := make(map[FieldKey]struct{}, len(base.keys))
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
