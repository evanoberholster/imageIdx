package bitmapidx

import (
	"context"
	"errors"
	common "github.com/evanoberholster/imageIdx/common"
	"sync"
	"sync/atomic"
	"time"

	commitqueue "github.com/evanoberholster/imageIdx/internal/commit"
	"github.com/evanoberholster/imageIdx/internal/storage"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
)

var (
	// ErrInvalidMutation reports a malformed membership transition.
	ErrInvalidMutation = errors.New("bitmapidx: invalid mutation")
	// ErrInvalidImageID reports ImageID zero, which is reserved.
	ErrInvalidImageID = errors.New("bitmapidx: image ID zero is reserved")
	// ErrDuplicateImageID reports an image already holding a value in a namespace
	// that allows only one value per image.
	ErrDuplicateImageID = errors.New("bitmapidx: duplicate image ID")
)

// NamespaceConfig controls namespace delta rotation and compaction. The zero
// value is valid; NewNamespaceIndex fills defaults. ActiveTarget rotates
// Active to Frozen and schedules background compaction; HardDeltaLimit
// and MaxDeltaValues instead fold the batch directly into Base on the
// write path, bounding every reader-visible delta.
type NamespaceConfig struct {
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
	// DefaultNamespaceActiveTarget is the default Active-to-Frozen rotation bound.
	DefaultNamespaceActiveTarget = 2_048
	// DefaultNamespaceHardDeltaLimit is the maximum default visible delta size.
	DefaultNamespaceHardDeltaLimit = 4_096
	// DefaultNamespaceMaxDeltaValues is the default distinct-value delta bound.
	DefaultNamespaceMaxDeltaValues = 256
)

func (c NamespaceConfig) withDefaults() (NamespaceConfig, error) {
	if c.ActiveTarget < 0 || c.HardDeltaLimit < 0 || c.MaxDeltaValues < 0 || c.CompactDelay < 0 {
		return NamespaceConfig{}, ErrInvalidConfig
	}
	if c.ActiveTarget == 0 {
		c.ActiveTarget = DefaultNamespaceActiveTarget
	}
	if c.HardDeltaLimit == 0 {
		c.HardDeltaLimit = DefaultNamespaceHardDeltaLimit
	}
	if c.MaxDeltaValues == 0 {
		c.MaxDeltaValues = DefaultNamespaceMaxDeltaValues
	}
	if c.HardDeltaLimit < 2 || c.ActiveTarget >= c.HardDeltaLimit {
		return NamespaceConfig{}, ErrInvalidConfig
	}
	return c, nil
}

// NamespaceMetadata is a copied summary of the current namespace snapshot:
// the immutable definition, the monotonically increasing view
// generation, and the number of values in Base.
type NamespaceMetadata struct {
	Spec       NamespaceSpec
	Generation uint64
	BaseValues uint64
}

// NamespaceIndex is one immutable-namespace, multi-value index. Readers load one
// atomically published namespaceView and take no locks beyond the reader
// tracker; writers stage Pebble-style batches that commit through one
// background pipeline per index, coalescing concurrent batches into a
// group sharing one lock hold, one WAL sync, and one view publication.
// One batch publishes atomically across postings and dictionary.
//
// Reads against a closed index report empty results, as do queries
// compiled after close; writes and maintenance report ErrClosed. The zero
// value is not usable; construct with NewNamespaceIndex. Store-owned indexes
// (owner != nil) route writes through the store WAL and must be closed via
// the store.
type NamespaceIndex struct {
	owner    *Index // immutable before exposing the handle
	view     atomic.Pointer[namespaceView]
	readers  *storage.ReaderTracker
	writerMu sync.Mutex
	config   NamespaceConfig
	closed   atomic.Bool
	workers  sync.WaitGroup
	pending  atomic.Int64
	dict     dictionaryWriter
	analyzer *textanalyzer.Analyzer
	// Commit pipeline: prepared batches and barrier markers queue here;
	// the shared commit.Queue collects each burst into one group that the
	// loop commits under a single lock hold. barrierErr latches the first
	// commit error since the previous barrier and is touched only by the
	// loop goroutine.
	commits    *commitqueue.Queue[commitReq]
	barrierErr error
}

// NewNamespaceIndex constructs an empty namespace index from a validated
// definition. Config zero values take defaults. The definition is
// deep-copied; text namespaces also build their analyzer here so writes and
// queries never construct one per batch.
func NewNamespaceIndex(namespace NamespaceSpec, config NamespaceConfig) (*NamespaceIndex, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	namespace.Normalizer = namespace.Normalizer.clone()
	namespace.Analyzer = namespace.Analyzer.clone()
	index := &NamespaceIndex{
		config:  config,
		readers: &storage.ReaderTracker{},
	}
	index.commits = commitqueue.New(commitQueueCapacity, index.commitGroup)
	if namespace.Kind == common.KindBitmapString {
		index.analyzer, err = textanalyzer.New(namespace.Analyzer.Code, namespace.Analyzer.Version, namespace.Analyzer.Params)
		if err != nil {
			return nil, err
		}
	}
	index.view.Store(&namespaceView{namespace: namespace, dict: index.dict.view(), generation: 1})
	return index, nil
}

// Spec returns a deep copy of the immutable namespace definition.
func (i *NamespaceIndex) Spec() NamespaceSpec { return i.view.Load().namespace.clone() }

// spec returns the current immutable definition without copying. Internal
// paths only read it; callers that may retain or mutate the definition use
// the cloning Spec method.
func (i *NamespaceIndex) spec() NamespaceSpec { return i.view.Load().namespace }

// Metadata returns a copied summary of the current snapshot.
func (i *NamespaceIndex) Metadata() NamespaceMetadata {
	view := i.view.Load()
	return NamespaceMetadata{Spec: view.namespace.clone(), Generation: view.generation, BaseValues: uint64(len(view.base.keys))}
}

// PendingCompactions reports the number of in-flight background
// compactions folding Frozen into Base.
func (i *NamespaceIndex) PendingCompactions() int64 { return i.pending.Load() }

// WaitIdle blocks until background compactions drain or ctx is cancelled.
// It polls every 100µs via storage.WaitForCondition and never takes the writer lock.
func (i *NamespaceIndex) WaitIdle(ctx context.Context) error {
	return storage.WaitForCondition(ctx, func() bool { return i.pending.Load() == 0 })
}

// Contains reports whether id holds value in this namespace. Unknown values
// report false with no error; kind mismatches and oversized values report
// errors. Closed indexes report false.
func (i *NamespaceIndex) Contains(id ImageID, value Value) (bool, error) {
	base, canonical, err := i.keyForValue(i.spec(), value)
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
func (i *NamespaceIndex) ContainsKey(id ImageID, key NamespaceKey) bool {
	token := i.readers.Acquire()
	view := i.view.Load()
	present := !i.closed.Load() && view.contains(key, id)
	i.readers.Release(token)
	return present
}

// Writes stage through Batch and commit through the background pipeline;
// see batch.go and commit.go. A batch queued with Batch.Apply becomes
// visible after Barrier.

// Compact synchronously merges Frozen and Active into a new Base and
// seals the dictionary. It preserves the visible value set, so only
// posting storage moves.
func (i *NamespaceIndex) Compact() error {
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

// Vacuum compacts the namespace and drops dictionary values with no
// remaining posting. It is the only operation that removes dead
// dictionary entries, and the only one that advances the dictionary
// generation without new values.
func (i *NamespaceIndex) Vacuum() error {
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

// Close marks a standalone namespace closed, waits for compactions and prior
// readers, and rejects further writes. Store-owned indexes reject Close;
// close the store instead.
func (i *NamespaceIndex) Close() error {
	if i.owner != nil {
		return ErrOwnedIndex
	}
	return i.closeInternal()
}

func (i *NamespaceIndex) closeInternal() error {
	// Stop the commit queue first so queued batches commit before the
	// writer locks below are taken: the loop's final groups need
	// writerMu, which close holds only briefly here.
	i.commits.Stop()
	i.commits.Wait()
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
func (i *NamespaceIndex) LookupValue(dst []byte, key NamespaceKey) ([]byte, bool) {
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
func (i *NamespaceIndex) Iterate(fn func(key NamespaceKey, value []byte) bool) {
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
func (i *NamespaceIndex) Prefix(prefix []byte, fn func(key NamespaceKey, value []byte) bool) {
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
func (i *NamespaceIndex) KeyForValue(value Value) (NamespaceKey, bool, error) {
	namespace := i.spec()
	key, canonical, err := i.keyForValue(namespace, value)
	if err != nil {
		return NamespaceKey{}, false, err
	}
	if canonical == nil {
		return key, true, nil // continuous values have no dictionary entry
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return NamespaceKey{}, false, nil
	}
	resolved, ok := view.dict.resolve(key, canonical)
	i.readers.Release(token)
	if !ok {
		return key, false, nil
	}
	return resolved, true, nil
}
