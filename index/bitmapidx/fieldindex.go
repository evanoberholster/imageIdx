package bitmapidx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
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
	owner    *Index // immutable before exposing the handle
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

// field returns the current immutable definition without copying. Internal
// paths only read it; callers that may retain or mutate the definition use
// the cloning Field method.
func (i *FieldIndex) field() Field { return i.view.Load().field }

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
	base, canonical, err := i.keyForValue(i.field(), value)
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
	if i.field().Kind == KindText {
		return ErrUnsupportedOperation
	}
	return i.applyPrepared(FieldMutation{Add: entries})
}

// Remove applies membership removals atomically. Unknown removals are
// no-ops and never create dictionary entries. Text fields reject Remove;
// use ReplaceText.
func (i *FieldIndex) Remove(entries ...Entry) error {
	if i.field().Kind == KindText {
		return ErrUnsupportedOperation
	}
	return i.applyPrepared(FieldMutation{Remove: entries})
}

// Apply atomically applies one membership batch with remove-wins
// semantics for the same (key, ImageID). Text fields reject Apply; use
// ReplaceText.
func (i *FieldIndex) Apply(mutation FieldMutation) error {
	if i.field().Kind == KindText {
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
	field := i.field()
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
// Each image may appear at most once per batch; a repeated ImageID has
// ambiguous sequential semantics and is rejected with ErrDuplicateImageID.
// Store-owned batches pre-check source bytes against a conservative
// fanout bound; the WAL encoder enforces the exact membership limit.
func (i *FieldIndex) ReplaceText(changes ...TextReplacement) error {
	field := i.field()
	if field.Kind != KindText {
		return ErrFieldMismatch
	}
	seen := make(map[ImageID]struct{}, len(changes))
	for _, change := range changes {
		if change.ImageID == 0 {
			return ErrInvalidImageID
		}
		if _, ok := seen[change.ImageID]; ok {
			return ErrDuplicateImageID
		}
		seen[change.ImageID] = struct{}{}
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
// call. It materializes merged state, so it allocates. The reader guard is
// released even if fn panics; fn must not call maintenance operations.
func (i *FieldIndex) Iterate(fn func(key FieldKey, value []byte) bool) {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if !i.closed.Load() {
		view.dict.iterate(fn)
	}
}

// Prefix visits dictionary values starting with prefix, in byte order of
// canonical values. Values alias storage valid only for the call. It
// re-sorts by value, so it is O(N log N); see dictionaryView.prefix. The
// reader guard is released even if fn panics; fn must not call maintenance
// operations.
func (i *FieldIndex) Prefix(prefix []byte, fn func(key FieldKey, value []byte) bool) {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if !i.closed.Load() {
		view.dict.prefix(prefix, fn)
	}
}

// KeyForValue canonicalizes and hashes value, returning its dictionary
// key including its collision slot. Absent values, and any value on a
// closed index, report ok=false. Continuous values have no dictionary
// entry and report their order key directly.
func (i *FieldIndex) KeyForValue(value Value) (FieldKey, bool, error) {
	field := i.field()
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
