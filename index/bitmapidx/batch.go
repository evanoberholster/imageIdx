package bitmapidx

import (
	"errors"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
)

var (
	// ErrBatchInFlight reports Apply or Reset on a batch with an Apply
	// already in flight. A Batch serves one caller at a time.
	ErrBatchInFlight = errors.New("bitmapidx: batch apply already in flight")
	// ErrBatchClosed reports use of a batch after Close.
	ErrBatchClosed = errors.New("bitmapidx: batch is closed")
)

// Batch accumulates membership mutations for one namespace and commits them
// through the index commit pipeline, Pebble-style: the caller stages Add,
// Remove, Move, and ReplaceText operations, Apply enqueues the prepared
// batch and returns once it is queued, and Barrier waits until every
// batch queued before it is WAL-appended and published.
//
// Ownership: staged entries borrow caller value bytes exactly like the
// former EnumBytesValue contract — callers must not mutate them until Apply
// returns. Apply copies each distinct canonical into committer-owned
// storage before enqueueing, so once Apply returns the batch (and the
// caller buffers) may be Reset and reused immediately, even though the
// commit itself still runs in the background. Move values are int64s and
// TextReplacement strings are immutable, so both are retained by
// reference. A Batch serves one caller at a time; Reset reuses its
// buffers and Close invalidates it.
type Batch struct {
	index        *Index
	add          []Entry
	remove       []Entry
	moves        []Move
	replacements []TextReplacement
	inFlight     atomic.Bool
	sealed       atomic.Bool
}

// NewBatch returns an empty batch bound to i. The batch is writer-owned:
// stage operations from one goroutine, then Apply. A batch may be reused
// with Reset after Apply returns.
func (i *Index) NewBatch() *Batch {
	return &Batch{index: i}
}

// Add stages membership additions. Already-present memberships are
// no-ops at commit. Text namespaces reject Add; use ReplaceText.
func (b *Batch) Add(entries ...Entry) error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	namespace := b.index.spec()
	if namespace.Kind == common.KindBitmapString {
		return ErrUnsupportedOperation
	}
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		if entry.Value.Kind != namespace.Kind {
			return ErrValueKind
		}
		b.add = append(b.add, entry)
	}
	return nil
}

// Remove stages membership removals. Unknown removals are no-ops at
// commit and never create dictionary entries. Text namespaces reject Remove;
// use ReplaceText.
func (b *Batch) Remove(entries ...Entry) error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	namespace := b.index.spec()
	if namespace.Kind == common.KindBitmapString {
		return ErrUnsupportedOperation
	}
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		if entry.Value.Kind != namespace.Kind {
			return ErrValueKind
		}
		b.remove = append(b.remove, entry)
	}
	return nil
}

// Move stages value transitions for continuous and geo namespaces.
// Duplicate images in one batch are rejected, no-op moves are dropped,
// and stale From values fail at commit.
func (b *Batch) Move(moves ...Move) error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	namespace := b.index.spec()
	if namespace.Kind != common.KindBitmapContinuous && namespace.Kind != common.KindBitmapGeo {
		return ErrNamespaceMismatch
	}
	for _, move := range moves {
		if move.ImageID == 0 {
			return ErrInvalidImageID
		}
		for _, staged := range b.moves {
			if staged.ImageID == move.ImageID {
				return ErrInvalidMutation
			}
		}
		if move.From == move.To {
			continue
		}
		b.moves = append(b.moves, move)
	}
	return nil
}

// ReplaceText stages complete logical text-namespace replacements. Each image
// may appear at most once per batch; a repeated ImageID has ambiguous
// sequential semantics and is rejected with ErrDuplicateImageID.
func (b *Batch) ReplaceText(changes ...TextReplacement) error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	if b.index.spec().Kind != common.KindBitmapString {
		return ErrNamespaceMismatch
	}
	for _, change := range changes {
		if change.ImageID == 0 {
			return ErrInvalidImageID
		}
		for _, staged := range b.replacements {
			if staged.ImageID == change.ImageID {
				return ErrDuplicateImageID
			}
		}
		b.replacements = append(b.replacements, change)
	}
	return nil
}

// Len reports the number of staged operations, counting a move and a text
// replacement once each.
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return len(b.add) + len(b.remove) + len(b.moves) + len(b.replacements)
}

// Empty reports whether the batch stages nothing.
func (b *Batch) Empty() bool {
	return b.Len() == 0
}

// Reset clears staged operations while retaining buffer capacity. It
// reports ErrBatchInFlight while an Apply is in flight and ErrBatchClosed
// after Close.
func (b *Batch) Reset() error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	if b.inFlight.Load() {
		return ErrBatchInFlight
	}
	b.add = b.add[:0]
	b.remove = b.remove[:0]
	b.moves = b.moves[:0]
	b.replacements = b.replacements[:0]
	return nil
}

// Close invalidates the batch and releases its buffers. It is idempotent.
func (b *Batch) Close() error {
	if b.sealed.Swap(true) {
		return nil
	}
	b.add = nil
	b.remove = nil
	b.moves = nil
	b.replacements = nil
	return nil
}

// Apply prepares the staged operations and enqueues them on the index
// commit pipeline, returning once the batch is queued. Queued is not
// committed: call Barrier to wait until every batch queued before it is
// WAL-appended and published (durable under DurabilitySync).
//
// Preparation — move expansion, text analysis, canonicalization — runs in
// the caller, so analyzer and hashing work stays concurrent across
// writers; only the commit itself serializes. An empty batch is a no-op
// that enqueues nothing.
func (b *Batch) Apply() error {
	if b.sealed.Load() {
		return ErrBatchClosed
	}
	if !b.inFlight.CompareAndSwap(false, true) {
		return ErrBatchInFlight
	}
	defer b.inFlight.Store(false)
	i := b.index
	if i.closed.Load() {
		return ErrClosed
	}
	if i.owner != nil && len(b.replacements) != 0 {
		remaining := MaxNamespaceBatchBytes
		if len(b.replacements) > MaxNamespaceBatchUpdates*batchFanoutBound {
			return ErrBatchTooLarge
		}
		for _, change := range b.replacements {
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
	mutation, err := b.expand()
	if err != nil {
		return err
	}
	if len(mutation.Add) == 0 && len(mutation.Remove) == 0 {
		return nil
	}
	changes, err := i.prepare(i.spec(), mutation)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	// Detach staged canonicals from caller and batch memory: the commit
	// runs after Apply returns, so each distinct value gets one
	// committer-owned copy here. ID columns are already freshly built
	// by preparation.
	for n := range changes {
		if len(changes[n].canonical) == 0 {
			continue
		}
		owned := make([]byte, len(changes[n].canonical))
		copy(owned, changes[n].canonical)
		changes[n].canonical = owned
	}
	if !i.commits.Submit(commitReq{changes: changes}) {
		return ErrClosed
	}
	return nil
}

// expand folds staged moves and text replacements into entries. It runs
// in the caller during Apply, matching the former Move/ReplaceText
// validation and analysis order.
func (b *Batch) expand() (namespaceMutation, error) {
	mutation := namespaceMutation{Add: b.add, Remove: b.remove}
	namespace := b.index.spec()
	value := func(v int64) Value {
		if namespace.Kind == common.KindBitmapGeo {
			return GeoValue(uint64(v))
		}
		return IntValue(v)
	}
	for _, move := range b.moves {
		if move.From != 0 {
			mutation.Remove = append(mutation.Remove, Entry{ImageID: move.ImageID, Value: value(move.From)})
		}
		if move.To != 0 {
			mutation.Add = append(mutation.Add, Entry{ImageID: move.ImageID, Value: value(move.To)})
		}
	}
	for _, change := range b.replacements {
		before, err := analyzeSet(b.index.analyzer, change.Before)
		if err != nil {
			return namespaceMutation{}, err
		}
		after, err := analyzeSet(b.index.analyzer, change.After)
		if err != nil {
			return namespaceMutation{}, err
		}
		for token := range before {
			if _, ok := after[token]; ok {
				continue
			}
			mutation.Remove = append(mutation.Remove, Entry{ImageID: change.ImageID, Value: Value{Kind: common.KindBitmapString, Bytes: []byte(token)}})
		}
		for token := range after {
			if _, ok := before[token]; ok {
				continue
			}
			mutation.Add = append(mutation.Add, Entry{ImageID: change.ImageID, Value: Value{Kind: common.KindBitmapString, Bytes: []byte(token)}})
		}
	}
	return mutation, nil
}
