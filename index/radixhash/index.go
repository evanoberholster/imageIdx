package radixhash

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// parallelMinEntries is the batch size above which independent root buckets are
// rebuilt concurrently. Below it the goroutine overhead outweighs the gain.
const parallelMinEntries = 4096

// indexView is one immutable, atomically published snapshot of the index.
// Nothing reachable from a published view is ever mutated; a batch builds
// replacement leaves and nodes before publishing. arena is carried in the view
// so an arena swap (for example by Compact or a file-backed reopen) is safe for
// readers that pinned the previous view.
type indexView struct {
	generation  uint64
	root        *[radixFanout]uint32 // root radix node, indexed by Hash[0]
	arena       []byte
	entries     uint64
	rootBuckets uint64
	depth       uint8
}

// Index is a byte-oriented radix hash index over a fixed arena. Writers are
// serialized by an internal mutex; readers are lock-free and only observe
// published views.
//
// An Index is safe for concurrent use by any number of readers and one writer.
type Index struct {
	alloc   *pagealloc.Allocator
	config  Config
	tracker storage.ReaderTracker

	// release, when non-nil, unmaps and closes a file-backed arena on Close.
	release func() error
	// sync, when non-nil, flushes a file-backed arena to disk.
	sync func() error
	// persistent is true for a file-backed, check-pointable arena.
	persistent bool
	// slot is the active superblock slot (0 or 1).
	slot int

	mu     sync.Mutex
	view   atomic.Pointer[indexView]
	closed atomic.Bool

	// records is the writer-owned grouping scratch used by Apply. It is only
	// touched under mu, grows once, and is reused so a small batch allocates
	// nothing to group records by root bucket.
	records []slot
	// partition holds the reusable per-level 256-way partition buffers. Like
	// records it is writer-owned and only touched under mu.
	partition partitionScratch
	// workScratch holds per-worker partition buffers for the large-batch
	// fan-out, reused across Apply calls. It is writer-owned and only touched
	// under mu.
	workScratch []partitionScratch
}

// New allocates a heap-backed arena of approximately arenaBytes and returns an
// empty index. pagealloc rounds the arena down to a power-of-two number of
// MinBlockSize blocks, so Metadata reports the usable size.
func New(arenaBytes uint64, config Config) (*Index, error) {
	allocator, err := pagealloc.New(arenaBytes)
	if err != nil {
		return nil, err
	}
	return newIndex(allocator, make([]byte, allocator.TotalBytes()), config)
}

// NewWithArena returns an empty index over a caller-owned arena. The arena may
// be mmap-backed; the index only uses the leading TotalBytes rounded by
// pagealloc. The arena must not be shared with another allocator.
func NewWithArena(arena []byte, config Config) (*Index, error) {
	allocator, err := pagealloc.New(uint64(len(arena)))
	if err != nil {
		return nil, err
	}
	return newIndex(allocator, arena, config)
}

// newIndex publishes the first view with an empty in-memory root node. The
// root lives in the view (not the arena) so a publish does not allocate and a
// lookup saves one random dereference; persistence will move it to the
// superblock.
func newIndex(allocator *pagealloc.Allocator, arena []byte, config Config) (*Index, error) {
	index := &Index{alloc: allocator, config: config.withDefaults()}
	index.view.Store(&indexView{generation: 1, root: &[radixFanout]uint32{}, arena: arena})
	return index, nil
}

// extent is one pagealloc allocation recorded by a batch transaction.
type extent struct {
	ref   pagealloc.Ref
	bytes uint32
}

// txn tracks the replacements of one Apply. Allocated extents are recorded so
// they can be freed if the batch fails; replaced extents are retired only after
// the new view is published, so a failure never invalidates the live view.
// alloc and retire serialize on mu so independent root buckets can rebuild in
// parallel while the allocator stays single-owner.
type txn struct {
	index       *Index
	arena       []byte
	mu          sync.Mutex
	newExtents  []extent
	retirements []extent
	delta       atomic.Int64
	maxDepth    atomic.Int64
}

func (t *txn) alloc(size uint32) (pagealloc.Ref, uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ref, capacity, err := t.index.alloc.Alloc(size)
	if err != nil {
		return 0, 0, err
	}
	t.newExtents = append(t.newExtents, extent{ref: ref, bytes: capacity})
	return ref, capacity, nil
}

func (t *txn) retire(ref pagealloc.Ref, bytes uint32) {
	t.mu.Lock()
	t.retirements = append(t.retirements, extent{ref: ref, bytes: bytes})
	t.mu.Unlock()
}

func (t *txn) addDelta(delta int64) { t.delta.Add(delta) }

func (t *txn) noteDepth(level int) {
	for {
		current := t.maxDepth.Load()
		if int64(level) <= current || t.maxDepth.CompareAndSwap(current, int64(level)) {
			return
		}
	}
}

func (t *txn) rollback() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, replacement := range t.newExtents {
		_ = t.index.alloc.Free(replacement.ref, replacement.bytes)
	}
}

// Apply atomically inserts every entry as one batch. Each affected leaf is read
// once, merged, and rewritten into a fresh extent; interior nodes on the changed
// paths (and the root) are path-copied once. Duplicate (hash, image) pairs
// collapse, and a batch that changes nothing publishes no generation.
//
// ImageIdx normally applies at most about 64 records per batch, so the common
// path is deliberately serial and allocation-light: records are counting-sorted
// into one reusable scratch and each affected root bucket is rebuilt once. No
// map, goroutine, channel, or worker pool is created for a small batch. Larger
// batches (compaction/rebuild) may still fan out across buckets.
func (i *Index) Apply(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidEntry
		}
	}

	// Group mutations by root byte; the carried tail starts at Hash[1].
	var counts [radixFanout]int
	records := i.groupByRoot(entries, &counts)

	current := i.view.Load()
	oldRoot := current.root
	batch := &txn{index: i, arena: current.arena}
	nextRoot := *oldRoot // copy-on-write the 1 KiB root

	parallel := len(entries) >= parallelMinEntries
	if err := batch.applyBuckets(records, &counts, oldRoot, &nextRoot, parallel); err != nil {
		batch.rollback()
		return err
	}
	if nextRoot == *oldRoot {
		return nil
	}

	i.view.Store(&indexView{
		generation:  current.generation + 1,
		root:        &nextRoot,
		arena:       current.arena,
		entries:     uint64(int64(current.entries) + batch.delta.Load()),
		rootBuckets: countRootBuckets(&nextRoot),
		depth:       maxUint8(current.depth, uint8(batch.maxDepth.Load())),
	})
	// Retirements happen after publication under the writer lock; the extents
	// remain allocated until a later Reclaim proves no reader needs them.
	for _, replaced := range batch.retirements {
		_ = i.alloc.Retire(replaced.ref, replaced.bytes)
	}
	// Optional prompt reclamation bounds dead space during sustained writes.
	if i.config.AutoReclaimBytes > 0 && i.alloc.RetiredBytes() >= i.config.AutoReclaimBytes {
		i.tracker.WaitForPriorReaders()
		_ = i.alloc.Reclaim()
	}
	return nil
}

// Remove atomically deletes the given (hash, ImageID) associations. It follows
// the same copy-on-write protocol as Apply: only the affected leaves and the
// interior nodes on their paths are replaced, a leaf that loses its last record
// is detached, and an interior node with no remaining children is pruned.
// Removing a pair that is not present publishes no generation, so retries are
// idempotent. Duplicate hashes stay supported: only the named (hash, ImageID)
// pair is removed and other ImageIDs for the same hash remain.
func (i *Index) Remove(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidEntry
		}
	}

	var counts [radixFanout]int
	records := i.groupByRoot(entries, &counts)
	var starts [radixFanout]int
	sum := 0
	for key := 0; key < radixFanout; key++ {
		starts[key] = sum
		sum += counts[key]
	}

	current := i.view.Load()
	oldRoot := current.root
	batch := &txn{index: i, arena: current.arena}
	nextRoot := *oldRoot
	scratch := &i.partition
	changed := false
	for key := 0; key < radixFanout; key++ {
		if counts[key] == 0 {
			continue
		}
		updated, err := batch.removeChild(oldRoot[key], 1, records[starts[key]:starts[key]+counts[key]], scratch)
		if err != nil {
			batch.rollback()
			return err
		}
		if updated != oldRoot[key] {
			nextRoot[key] = updated
			changed = true
		}
	}
	if !changed {
		return nil
	}
	i.view.Store(&indexView{
		generation:  current.generation + 1,
		root:        &nextRoot,
		arena:       current.arena,
		entries:     uint64(int64(current.entries) + batch.delta.Load()),
		rootBuckets: countRootBuckets(&nextRoot),
		depth:       maxUint8(current.depth, uint8(batch.maxDepth.Load())),
	})
	for _, replaced := range batch.retirements {
		_ = i.alloc.Retire(replaced.ref, replaced.bytes)
	}
	if i.config.AutoReclaimBytes > 0 && i.alloc.RetiredBytes() >= i.config.AutoReclaimBytes {
		i.tracker.WaitForPriorReaders()
		_ = i.alloc.Reclaim()
	}
	return nil
}

// removeChild returns the replacement child reference after deleting every
// removal whose tail starts at level. It returns oldChild unchanged when
// nothing matched and 0 when the subtree becomes empty.
func (t *txn) removeChild(oldChild uint32, level int, removals []slot, scratch *partitionScratch) (uint32, error) {
	if oldChild == 0 {
		return 0, nil
	}
	tailLen := tailLenFor(level)
	ref, isNode := decodeChild(oldChild)
	if isNode {
		if !validExtent(t.arena, ref, radixNodeBytes) {
			return 0, fmt.Errorf("%w: node extent outside arena", ErrCorrupt)
		}
		var old [radixFanout]uint32
		readNode(t.arena, ref, &old)
		var counts [radixFanout]uint32
		var offsets [radixFanout + 1]uint32
		out := scratch.buffer(level, len(removals))
		partitionSlots(out, removals, tailLen, &counts, &offsets)

		var next [radixFanout]uint32
		changed := false
		nonEmpty := false
		for key := 0; key < radixFanout; key++ {
			child := old[key]
			if counts[key] != 0 {
				updated, err := t.removeChild(child, level+1, out[offsets[key]:offsets[key+1]], scratch)
				if err != nil {
					return 0, err
				}
				if updated != child {
					if !changed {
						next = old
						changed = true
					}
					next[key] = updated
					child = updated
				}
			}
			if child != 0 {
				nonEmpty = true
			}
		}
		if !changed {
			return oldChild, nil
		}
		if !nonEmpty {
			t.retire(ref, radixNodeBytes)
			return 0, nil
		}
		newRef, _, err := t.alloc(radixNodeBytes)
		if err != nil {
			return 0, err
		}
		if err := writeNode(t.arena, newRef, &next); err != nil {
			return 0, err
		}
		t.retire(ref, radixNodeBytes)
		return encodeChild(newRef, true), nil
	}

	header, err := readLeafHeader(t.arena, ref)
	if err != nil {
		return 0, err
	}
	if header.depth != level {
		return 0, fmt.Errorf("%w: leaf depth %d reached at level %d", ErrCorrupt, header.depth, level)
	}
	existing, _, err := readLeafSlotsInto(scratch.existing[:0], t.arena, ref)
	if err != nil {
		return 0, err
	}
	scratch.existing = existing
	removals = dedupeSlots(removals, tailLen)

	keep := existing[:0]
	ri := 0
	removed := 0
	for n := range existing {
		s := &existing[n]
		for ri < len(removals) && compareSlot(&removals[ri], s, tailLen) < 0 {
			ri++
		}
		if ri < len(removals) && sameSlot(&removals[ri], s, tailLen) {
			removed++
			continue
		}
		keep = append(keep, *s)
	}
	if removed == 0 {
		return oldChild, nil
	}
	if len(keep) == 0 {
		t.retire(ref, header.extentBytes)
		t.addDelta(-int64(header.count))
		return 0, nil
	}
	return t.writeLeaf(level, keep, ref, header.extentBytes, header.count)
}

// groupByRoot copies entries into the reusable writer scratch ordered by
// Hash[0] and fills counts with the per-bucket record count. Each root bucket
// is then one contiguous run, so the caller never builds a map and never grows
// a per-bucket slice. The scratch only grows, so the common small batch
// allocates nothing.
func (i *Index) groupByRoot(entries []Entry, counts *[radixFanout]int) []slot {
	for _, entry := range entries {
		counts[entry.Hash[0]]++
	}
	var starts [radixFanout]int
	sum := 0
	for key := 0; key < radixFanout; key++ {
		starts[key] = sum
		sum += counts[key]
	}
	if cap(i.records) < len(entries) {
		i.records = make([]slot, len(entries))
	}
	records := i.records[:len(entries)]
	for _, entry := range entries {
		key := entry.Hash[0]
		rec := &records[starts[key]]
		starts[key]++
		*rec = slot{id: entry.ImageID}
		copy(rec.tail[:], entry.Hash[1:])
	}
	return records
}

// applyBuckets rebuilds each affected root bucket exactly once. Small batches
// run serially; large batches fan out across disjoint root buckets. Every leaf
// and node is still built copy-on-write and the caller publishes one view.
func (t *txn) applyBuckets(records []slot, counts *[radixFanout]int, oldRoot, nextRoot *[radixFanout]uint32, parallel bool) error {
	var starts [radixFanout]int
	sum := 0
	for key := 0; key < radixFanout; key++ {
		starts[key] = sum
		sum += counts[key]
	}
	rebuild := func(key int, scratch *partitionScratch) error {
		n := counts[key]
		if n == 0 {
			return nil
		}
		updated, err := t.rebuildChild(oldRoot[key], 1, records[starts[key]:starts[key]+n], scratch)
		if err != nil {
			return err
		}
		nextRoot[key] = updated
		return nil
	}

	if !parallel {
		scratch := &t.index.partition
		for key := 0; key < radixFanout; key++ {
			if err := rebuild(key, scratch); err != nil {
				return err
			}
		}
		return nil
	}

	workers := min(runtime.GOMAXPROCS(0), sum, 8)
	if workers < 2 {
		scratch := &t.index.partition
		for key := 0; key < radixFanout; key++ {
			if err := rebuild(key, scratch); err != nil {
				return err
			}
		}
		return nil
	}
	// Each worker owns a scratch so concurrent subtree rebuilds never share a
	// partition buffer. The buffers are reused across Apply calls. This fan-out
	// is only for large maintenance batches; the common <=64-record path stays
	// serial on the writer's scratch.
	if cap(t.index.workScratch) < workers {
		t.index.workScratch = make([]partitionScratch, workers)
	}
	scratches := t.index.workScratch[:workers]
	var next atomic.Int32
	var wait sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wait.Add(1)
		go func(w int) {
			defer wait.Done()
			scratch := &scratches[w]
			for {
				key := int(next.Add(1)) - 1
				if key >= radixFanout {
					return
				}
				if err := rebuild(key, scratch); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}
		}(w)
	}
	wait.Wait()
	return firstErr
}

// rebuildChild returns the replacement child reference for the subtree rooted
// at oldChild (level is the child's radix level). records carry the unconsumed
// tail starting at Hash[level]. An existing leaf is updated by merging directly
// from its encoded SoA form; only a promotion falls back to decoded scratch.
func (t *txn) rebuildChild(oldChild uint32, level int, records []slot, scratch *partitionScratch) (uint32, error) {
	if oldChild == 0 {
		return t.newSubtreeFromRecords(level, records, scratch)
	}
	ref, isNode := decodeChild(oldChild)
	if isNode {
		return t.rebuildNode(ref, level, records, scratch)
	}

	header, err := readLeafHeader(t.arena, ref)
	if err != nil {
		return 0, err
	}
	if header.depth != level {
		return 0, fmt.Errorf("%w: leaf depth %d reached at level %d", ErrCorrupt, header.depth, level)
	}
	// Sort and deduplicate only the small incoming group, then merge it directly
	// with the encoded leaf. The existing leaf is never materialized as []slot.
	tailLen := tailLenFor(level)
	records = dedupeSlots(records, tailLen)
	idBytes := header.idBytes
	if idBytes == id32Bytes && idBytesForSlots(records) == id64Bytes {
		idBytes = id64Bytes
	}
	resultCount, newCount := leafMergedCount(t.arena, extentOffset(ref), header, tailLen, records)
	if newCount == 0 {
		return oldChild, nil // every incoming record already present
	}
	if level < maxDepth && t.fits(level, idBytes, resultCount) {
		return t.writeMergedLeaf(ref, header, level, records, resultCount, idBytes)
	}
	// The merged leaf would exceed the promotion threshold: fall back to the
	// decode+merge scratch and repartition. Promotion is rare, so materializing
	// here is acceptable.
	existing, _, err := readLeafSlotsInto(scratch.existing[:0], t.arena, ref)
	if err != nil {
		return 0, err
	}
	scratch.existing = existing
	merged := mergeSlotsInto(scratch.merged[:0], existing, records, tailLen)
	scratch.merged = merged
	t.retire(ref, header.extentBytes)
	t.addDelta(-int64(len(existing)))
	return t.buildNodeFromSlots(level, merged, scratch)
}

// writeMergedLeaf allocates a replacement leaf extent, writes the direct
// SoA -> SoA merge of the old leaf and the incoming group into it, then retires
// the old leaf and accounts for the entry delta. added must already be sorted
// and deduplicated at tailLen and count must be the exact merged count.
func (t *txn) writeMergedLeaf(oldRef pagealloc.Ref, header leafHeader, level int, added []slot, count, idBytes int) (uint32, error) {
	size := uint32(sizeForCapacity(level, idBytes, count))
	ref, capacity, err := t.alloc(size)
	if err != nil {
		return 0, err
	}
	offset := extentOffset(ref)
	extent := t.arena[offset : offset+uint64(capacity)]
	slotCapacity := capacityForSize(level, idBytes, capacity)
	encodeLeafHeader(extent, level, idBytes, count, slotCapacity)
	writeMergedLeafRecords(extent, level, idBytes, count, slotCapacity, t.arena, extentOffset(oldRef), header, added)
	t.retire(oldRef, header.extentBytes)
	t.addDelta(int64(count - header.count))
	t.noteDepth(level)
	return encodeChild(ref, false), nil
}

// rebuildNode path-copies the radix node at ref, applying each per-byte group
// of records to the matching child. It returns the old reference unchanged when
// no child changes.
func (t *txn) rebuildNode(ref pagealloc.Ref, level int, records []slot, scratch *partitionScratch) (uint32, error) {
	if !validExtent(t.arena, ref, radixNodeBytes) {
		return 0, fmt.Errorf("%w: node extent outside arena", ErrCorrupt)
	}
	var old [radixFanout]uint32
	readNode(t.arena, ref, &old)

	var counts [radixFanout]uint32
	var offsets [radixFanout + 1]uint32
	out := scratch.buffer(level, len(records))
	partitionSlots(out, records, tailLenFor(level), &counts, &offsets)

	var next [radixFanout]uint32
	changed := false
	for key := 0; key < radixFanout; key++ {
		if counts[key] == 0 {
			continue
		}
		updated, err := t.rebuildChild(old[key], level+1, out[offsets[key]:offsets[key+1]], scratch)
		if err != nil {
			return 0, err
		}
		if updated != old[key] {
			if !changed {
				next = old
				changed = true
			}
			next[key] = updated
		}
	}
	if !changed {
		return encodeChild(ref, true), nil
	}
	newRef, _, err := t.alloc(radixNodeBytes)
	if err != nil {
		return 0, err
	}
	if err := writeNode(t.arena, newRef, &next); err != nil {
		return 0, err
	}
	t.retire(ref, radixNodeBytes)
	return encodeChild(newRef, true), nil
}

// newSubtreeFromRecords builds a fresh subtree for a child that does not exist
// yet. A small bucket becomes a leaf directly (Root[Hash[0]] -> leaf); once the
// records cross the promotion threshold the leaf is replaced by a radix node
// (Root[Hash[0]] -> Radix[Hash[1]] -> leaves), and the same adaptive promotion
// applies at deeper levels.
func (t *txn) newSubtreeFromRecords(level int, records []slot, scratch *partitionScratch) (uint32, error) {
	records = dedupeSlots(records, tailLenFor(level))
	if level == 1 && t.index.config.forceRootRadix {
		return t.buildNodeFromRecords(level, records, scratch)
	}
	if level >= maxDepth || t.fits(level, idBytesForSlots(records), len(records)) {
		return t.writeLeaf(level, records, 0, 0, 0)
	}
	return t.buildNodeFromRecords(level, records, scratch)
}

// buildNodeFromRecords creates a new radix node from records whose tail starts
// at level, distributing them across child subtrees.
func (t *txn) buildNodeFromRecords(level int, records []slot, scratch *partitionScratch) (uint32, error) {
	var counts [radixFanout]uint32
	var offsets [radixFanout + 1]uint32
	out := scratch.buffer(level, len(records))
	partitionSlots(out, records, tailLenFor(level), &counts, &offsets)

	var children [radixFanout]uint32
	for key := 0; key < radixFanout; key++ {
		if counts[key] == 0 {
			continue
		}
		child, err := t.newSubtreeFromRecords(level+1, out[offsets[key]:offsets[key+1]], scratch)
		if err != nil {
			return 0, err
		}
		children[key] = child
	}
	return t.writeNewNode(&children)
}

// buildNodeFromSlots is the promotion path: it turns an oversized leaf into a
// radix node and redistributes the leaf's slots, which already have their next
// unconsumed byte at tail[0].
func (t *txn) buildNodeFromSlots(level int, slots []slot, scratch *partitionScratch) (uint32, error) {
	var counts [radixFanout]uint32
	var offsets [radixFanout + 1]uint32
	out := scratch.buffer(level, len(slots))
	partitionSlots(out, slots, tailLenFor(level), &counts, &offsets)

	var children [radixFanout]uint32
	for key := 0; key < radixFanout; key++ {
		if counts[key] == 0 {
			continue
		}
		child, err := t.newSubtreeFromSlots(level+1, out[offsets[key]:offsets[key+1]], scratch)
		if err != nil {
			return 0, err
		}
		children[key] = child
	}
	return t.writeNewNode(&children)
}

// newSubtreeFromSlots creates a leaf, or recurses into another promotion when
// the child would still exceed the threshold.
func (t *txn) newSubtreeFromSlots(level int, slots []slot, scratch *partitionScratch) (uint32, error) {
	if level >= maxDepth || t.fits(level, idBytesForSlots(slots), len(slots)) {
		return t.writeLeaf(level, slots, 0, 0, 0)
	}
	return t.buildNodeFromSlots(level, slots, scratch)
}

// writeLeaf allocates and encodes a leaf, retires the replaced leaf when one is
// supplied, and accounts for the entry delta and maximum depth.
func (t *txn) writeLeaf(level int, slots []slot, oldRef pagealloc.Ref, oldBytes uint32, oldCount int) (uint32, error) {
	size := uint32(sizeForCapacity(level, idBytesForSlots(slots), len(slots)))
	ref, capacity, err := t.alloc(size)
	if err != nil {
		return 0, err
	}
	offset := extentOffset(ref)
	if err := encodeLeaf(t.arena[offset:offset+uint64(capacity)], level, slots); err != nil {
		return 0, err
	}
	if oldRef != 0 {
		t.retire(oldRef, oldBytes)
	}
	t.addDelta(int64(len(slots) - oldCount))
	t.noteDepth(level)
	return encodeChild(ref, false), nil
}

// writeNewNode allocates and writes a brand-new radix node.
func (t *txn) writeNewNode(children *[radixFanout]uint32) (uint32, error) {
	ref, _, err := t.alloc(radixNodeBytes)
	if err != nil {
		return 0, err
	}
	if err := writeNode(t.arena, ref, children); err != nil {
		return 0, err
	}
	return encodeChild(ref, true), nil
}

// fits reports whether a leaf of count slots at level stays within the
// configured byte and slot thresholds. It is a method so the policy lives in
// one place and both promotion paths consult the same bounds.
func (t *txn) fits(level, idBytes, count int) bool {
	if count <= 0 {
		return true
	}
	if maxSlots := int(t.index.config.MaxLeafSlots); maxSlots > 0 && count > maxSlots {
		return false
	}
	return sizeForCapacity(level, idBytes, count) <= int(t.index.config.PromotionBytes)
}

// traverse follows the radix path for h and returns the leaf reference and its
// depth, or ok=false on a miss. The root is a view-local array, so a lookup
// saves one random dereference before reaching the arena-resident L1 nodes.
func (i *Index) traverse(view *indexView, h Hash) (pagealloc.Ref, int, bool) {
	child := view.root[h[0]]
	if child == 0 {
		return 0, 0, false
	}
	level := 1
	for {
		ref, isNode := decodeChild(child)
		if !isNode {
			return ref, level, true
		}
		if level >= maxDepth || !validExtent(view.arena, ref, radixNodeBytes) {
			return 0, 0, false
		}
		child = nodeChild(view.arena, ref, h[level])
		if child == 0 {
			return 0, 0, false
		}
		level++
	}
}

// Lookup appends the ImageIDs stored for exactly h to dst, in ascending order,
// and returns the extended slice. A missing hash leaves dst unchanged.
func (i *Index) Lookup(dst []ImageID, h Hash) []ImageID {
	i.withView(func(view *indexView) {
		ref, depth, ok := i.traverse(view, h)
		if !ok {
			return
		}
		_ = scanLeaf(view.arena, ref, h, depth, func(id ImageID) bool {
			dst = append(dst, id)
			return true
		})
	})
	return dst
}

// LookupFiltered is Lookup with a liveness predicate. Only IDs for which live
// returns true are appended. A nil predicate accepts every ID.
func (i *Index) LookupFiltered(dst []ImageID, h Hash, live func(ImageID) bool) []ImageID {
	i.withView(func(view *indexView) {
		ref, depth, ok := i.traverse(view, h)
		if !ok {
			return
		}
		_ = scanLeaf(view.arena, ref, h, depth, func(id ImageID) bool {
			if live == nil || live(id) {
				dst = append(dst, id)
			}
			return true
		})
	})
	return dst
}

// Count returns the number of ImageIDs stored for exactly h.
func (i *Index) Count(h Hash) uint64 {
	var count uint64
	i.withView(func(view *indexView) {
		ref, depth, ok := i.traverse(view, h)
		if !ok {
			return
		}
		_ = scanLeaf(view.arena, ref, h, depth, func(ImageID) bool {
			count++
			return true
		})
	})
	return count
}

// Contains reports whether exactly h has at least one ImageID.
func (i *Index) Contains(h Hash) bool {
	found := false
	i.withView(func(view *indexView) {
		ref, depth, ok := i.traverse(view, h)
		if !ok {
			return
		}
		_ = scanLeaf(view.arena, ref, h, depth, func(ImageID) bool {
			found = true
			return false
		})
	})
	return found
}

// withView runs fn while pinning the current published view so that Reclaim,
// Compact, and Close cannot reuse or unmap an arena the reader may still touch.
//
// The order is strict: Acquire -> check closed -> load/use the view -> Release.
// Checking closed before Acquire is unsafe: the reader could observe false,
// Close could then set the flag, complete its grace period, and unmap before
// the Acquire registers the reader, letting the reader touch released memory.
// Acquiring first makes Close's WaitForPriorReaders wait for this reader.
func (i *Index) withView(fn func(*indexView)) {
	token := i.tracker.Acquire()
	if !i.closed.Load() {
		fn(i.view.Load())
	}
	i.tracker.Release(token)
}

// Generation returns the current published generation.
func (i *Index) Generation() uint64 { return i.view.Load().generation }

// Metadata returns a copied summary of the current view and allocator state.
// It takes the writer lock so the allocator figures are consistent with the
// published view.
func (i *Index) Metadata() Metadata {
	i.mu.Lock()
	defer i.mu.Unlock()
	view := i.view.Load()
	return Metadata{
		Generation:     view.generation,
		Entries:        view.entries,
		RootBuckets:    view.rootBuckets,
		Depth:          view.depth,
		ArenaBytes:     i.alloc.TotalBytes(),
		FreeBytes:      i.alloc.FreeBytes(),
		RetiredExtents: i.alloc.Retired(),
	}
}

// Reclaim returns every retired extent to the allocator. The ReaderTracker is
// the sole authority for memory lifetime: the writer waits for all readers that
// could still reference a retired page to drain and then frees them, so callers
// never track reader generations.
func (i *Index) Reclaim() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	i.tracker.WaitForPriorReaders()
	return i.alloc.Reclaim()
}

// Compact rebuilds the tree into a fresh, tightly packed arena and atomically
// publishes it, reclaiming dead space and fragmentation. The index is briefly
// double-resident while the replacement is built. It follows the arena lifetime
// invariant: build -> publish -> wait for prior readers -> release the old
// arena, so no reader can observe an unmapped arena. A file-backed index is
// compacted into a heap arena and loses its file backing, so callers that need
// persistence should checkpoint instead.
//
// Compaction is one logical publication regardless of how many internal Apply
// calls the streaming rebuild needs, so the replacement generation is always
// the current generation plus one. A corruption failure aborts before
// publication and leaves the live index unchanged. After publication the new
// index is authoritative; a release error means cleanup of the old backing
// resource failed and is returned without rolling the publication back.
func (i *Index) Compact() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	current := i.view.Load()
	if current.entries == 0 {
		return nil
	}

	// Stream the old immutable tree into the fresh arena in bounded batches, so
	// peak compaction memory is independent of index size and no []Entry
	// proportional to the index is ever built. Any corruption the walker finds
	// aborts the rebuild without publishing it.
	batchEntries := compactBatchEntries
	if batchEntries < 1 {
		batchEntries = 1
	}
	fresh, err := New(i.alloc.TotalBytes(), i.config)
	if err != nil {
		return err
	}
	batch := make([]Entry, batchEntries)
	walker := newEntryWalker(current)
	for {
		written, more, walkErr := walker.Next(batch)
		if walkErr != nil {
			_ = fresh.Close()
			return walkErr
		}
		if written > 0 {
			if err := fresh.Apply(batch[:written]); err != nil {
				_ = fresh.Close()
				return err
			}
		}
		if !more {
			break
		}
	}

	// Build is complete. Publish the replacement view first, then run the
	// reader grace period, and only after prior readers drain release the old
	// arena. Unmapping before publication + grace would fault a live reader.
	// The temporary index advanced its own generation once per internal Apply;
	// that is implementation work, not publication, so override it with the
	// single logical generation this compaction represents.
	release := i.release
	built := fresh.view.Load()
	i.alloc = fresh.alloc
	i.release = fresh.release
	i.sync = fresh.sync
	i.persistent = false
	i.slot = 0
	i.view.Store(&indexView{
		generation:  current.generation + 1,
		root:        built.root,
		arena:       built.arena,
		entries:     built.entries,
		rootBuckets: built.rootBuckets,
		depth:       built.depth,
	})
	fresh.alloc = nil
	fresh.release = nil
	fresh.sync = nil
	i.tracker.WaitForPriorReaders()
	if release != nil {
		if releaseErr := release(); releaseErr != nil {
			return releaseErr
		}
	}
	return nil
}

// Sync flushes a file-backed arena to disk. It is a no-op for heap arenas.
func (i *Index) Sync() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if i.sync == nil {
		return nil
	}
	return i.sync()
}

// Close marks the index closed, waits for in-flight readers to drain, then
// reclaims retired extents and unmaps any file-backed arena. It follows the
// arena lifetime invariant: prevent new operations -> wait for prior readers ->
// reclaim -> release. The closed flag is set before the grace period, and
// readers Acquire before checking it; checking closed before Acquire would let
// a reader register after the grace period and touch the released arena.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return nil
	}
	i.closed.Store(true)
	i.tracker.WaitForPriorReaders()
	err := i.alloc.Reclaim()
	if i.release != nil {
		if releaseErr := i.release(); err == nil {
			err = releaseErr
		}
		i.release = nil
		i.sync = nil
	}
	return err
}

// countRootBuckets counts non-empty root children.
func countRootBuckets(root *[radixFanout]uint32) uint64 {
	var count uint64
	for _, child := range root {
		if child != 0 {
			count++
		}
	}
	return count
}

// maxUint8 returns the larger of two depths.
func maxUint8(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}
