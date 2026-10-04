package radixhash

import (
	"encoding/binary"
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
	arena   []byte
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
	closed bool
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
	index := &Index{alloc: allocator, arena: arena, config: config.withDefaults()}
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
	generation  uint64
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
func (i *Index) Apply(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}

	// Group mutations by root byte; the carried tail starts at Hash[1].
	groups := make(map[byte][]slot, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidEntry
		}
		var record slot
		copy(record.tail[:], entry.Hash[1:])
		record.id = entry.ImageID
		groups[entry.Hash[0]] = append(groups[entry.Hash[0]], record)
	}

	current := i.view.Load()
	oldRoot := current.root

	batch := &txn{index: i, generation: current.generation}
	nextRoot := *oldRoot // copy-on-write the 1 KiB root
	workers := 1
	if len(entries) >= parallelMinEntries && len(groups) > 1 {
		workers = min(runtime.GOMAXPROCS(0), len(groups), 8)
	}
	if workers <= 1 {
		for key, records := range groups {
			updated, err := batch.rebuildChild(oldRoot[key], 1, records)
			if err != nil {
				batch.rollback()
				return err
			}
			nextRoot[key] = updated
		}
	} else {
		jobs := make(chan byte, len(groups))
		for key := range groups {
			jobs <- key
		}
		close(jobs)
		var wait sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		for w := 0; w < workers; w++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for key := range jobs {
					updated, err := batch.rebuildChild(oldRoot[key], 1, groups[key])
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						continue
					}
					nextRoot[key] = updated
				}
			}()
		}
		wait.Wait()
		if firstErr != nil {
			batch.rollback()
			return firstErr
		}
	}
	if nextRoot == *oldRoot {
		return nil
	}

	i.view.Store(&indexView{
		generation:  current.generation + 1,
		root:        &nextRoot,
		arena:       i.arena,
		entries:     uint64(int64(current.entries) + batch.delta.Load()),
		rootBuckets: countRootBuckets(&nextRoot),
		depth:       maxUint8(current.depth, uint8(batch.maxDepth.Load())),
	})
	// Retirements happen after publication under the writer lock; the extents
	// remain allocated until a later Reclaim proves no reader needs them.
	for _, replaced := range batch.retirements {
		_ = i.alloc.Retire(replaced.ref, replaced.bytes, current.generation)
	}
	// Optional prompt reclamation bounds dead space during sustained writes.
	if i.config.AutoReclaimBytes > 0 && i.alloc.RetiredBytes() >= i.config.AutoReclaimBytes {
		i.tracker.WaitForPriorReaders()
		_ = i.alloc.ReclaimAll()
	}
	return nil
}

// rebuildChild returns the replacement child reference for the subtree rooted
// at oldChild (level is the child's radix level). records carry the unconsumed
// tail starting at Hash[level].
func (t *txn) rebuildChild(oldChild uint32, level int, records []slot) (uint32, error) {
	if oldChild == 0 {
		return t.newSubtreeFromRecords(level, records)
	}
	ref, isNode := decodeChild(oldChild)
	if isNode {
		return t.rebuildNode(ref, level, records)
	}

	existing, header, err := readLeafSlots(t.index.arena, ref)
	if err != nil {
		return 0, err
	}
	if header.depth != level {
		return 0, fmt.Errorf("%w: leaf depth %d reached at level %d", ErrCorrupt, header.depth, level)
	}
	merged := mergeSlots(existing, records, tailLenFor(level))
	if len(merged) == len(existing) {
		return oldChild, nil // every record was already present
	}
	if level >= maxDepth || t.fits(level, idBytesForSlots(merged), len(merged)) {
		return t.writeLeaf(level, merged, ref, header.extentBytes, len(existing))
	}
	// Promote this individual leaf to a radix node consuming Hash[level].
	t.retire(ref, header.extentBytes)
	t.addDelta(-int64(len(existing)))
	return t.buildNodeFromSlots(level, merged)
}

// rebuildNode path-copies the radix node at ref, applying each per-byte group
// of records to the matching child. It returns the old reference unchanged when
// no child changes.
func (t *txn) rebuildNode(ref pagealloc.Ref, level int, records []slot) (uint32, error) {
	if !validExtent(t.index.arena, ref, radixNodeBytes) {
		return 0, fmt.Errorf("%w: node extent outside arena", ErrCorrupt)
	}
	var old [radixFanout]uint32
	readNode(t.index.arena, ref, &old)

	groups := partitionSlots(records, tailLenFor(level))
	var next [radixFanout]uint32
	changed := false
	for key, group := range groups {
		updated, err := t.rebuildChild(old[key], level+1, group)
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
	if err := writeNode(t.index.arena, newRef, &next); err != nil {
		return 0, err
	}
	t.retire(ref, radixNodeBytes)
	return encodeChild(newRef, true), nil
}

// newSubtreeFromRecords builds a fresh subtree for a child that does not exist
// yet. Level one is always a radix node so the normal shape is
// Root[Hash[0]] -> Radix[Hash[1]] -> leaf; deeper absences become leaves unless
// the records are large enough to promote immediately.
func (t *txn) newSubtreeFromRecords(level int, records []slot) (uint32, error) {
	records = dedupeSlots(records, tailLenFor(level))
	if level != 1 && (level >= maxDepth || t.fits(level, idBytesForSlots(records), len(records))) {
		return t.writeLeaf(level, records, 0, 0, 0)
	}
	return t.buildNodeFromRecords(level, records)
}

// buildNodeFromRecords creates a new radix node from records whose tail starts
// at level, distributing them across child subtrees.
func (t *txn) buildNodeFromRecords(level int, records []slot) (uint32, error) {
	groups := partitionSlots(records, tailLenFor(level))
	var children [radixFanout]uint32
	for key, group := range groups {
		child, err := t.newSubtreeFromRecords(level+1, group)
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
func (t *txn) buildNodeFromSlots(level int, slots []slot) (uint32, error) {
	groups := partitionSlots(slots, tailLenFor(level))
	var children [radixFanout]uint32
	for key, group := range groups {
		child, err := t.newSubtreeFromSlots(level+1, group)
		if err != nil {
			return 0, err
		}
		children[key] = child
	}
	return t.writeNewNode(&children)
}

// newSubtreeFromSlots creates a leaf, or recurses into another promotion when
// the child would still exceed the threshold.
func (t *txn) newSubtreeFromSlots(level int, slots []slot) (uint32, error) {
	if level >= maxDepth || t.fits(level, idBytesForSlots(slots), len(slots)) {
		return t.writeLeaf(level, slots, 0, 0, 0)
	}
	return t.buildNodeFromSlots(level, slots)
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
	if err := encodeLeaf(t.index.arena[offset:offset+uint64(capacity)], level, slots); err != nil {
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
	if err := writeNode(t.index.arena, ref, children); err != nil {
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

// withView runs fn while pinning the current published view so that Reclaim
// cannot reuse a page the reader may still touch.
func (i *Index) withView(fn func(*indexView)) {
	token := i.tracker.Acquire()
	fn(i.view.Load())
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

// Reclaim returns retired extents to the allocator once every reader still
// pinning a generation below oldestReaderGeneration has released its view.
// Callers that never call Reclaim retain replaced extents until Close.
func (i *Index) Reclaim(oldestReaderGeneration uint64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	return i.alloc.Reclaim(oldestReaderGeneration)
}

// ReclaimAll frees every retired extent. It is safe only when no reader can
// still hold an older view, such as after all reader activity has stopped.
func (i *Index) ReclaimAll() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	return i.alloc.ReclaimAll()
}

// Compact rebuilds the tree into a fresh, tightly packed arena and atomically
// publishes it, reclaiming dead space and fragmentation. The index is briefly
// double-resident while the replacement is built. A file-backed index is
// compacted into a heap arena and loses its file backing, so callers that need
// persistence should checkpoint instead.
func (i *Index) Compact() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	current := i.view.Load()
	if current.entries == 0 {
		return nil
	}
	entries := make([]Entry, 0, current.entries)
	i.collectEntries(current, &entries)

	fresh, err := New(i.alloc.TotalBytes(), i.config)
	if err != nil {
		return err
	}
	const chunk = 1 << 20
	for start := 0; start < len(entries); start += chunk {
		end := start + chunk
		if end > len(entries) {
			end = len(entries)
		}
		if err := fresh.Apply(entries[start:end]); err != nil {
			_ = fresh.Close()
			return err
		}
	}

	// Drain readers pinned to the old arena, then swap in the replacement.
	i.tracker.WaitForPriorReaders()
	release := i.release
	i.alloc = fresh.alloc
	i.arena = fresh.arena
	i.release = fresh.release
	i.sync = fresh.sync
	i.persistent = false
	i.slot = 0
	i.view.Store(fresh.view.Load())
	fresh.alloc = nil
	fresh.release = nil
	fresh.sync = nil
	if release != nil {
		_ = release()
	}
	return nil
}

// collectEntries walks the published tree and reconstructs every full hash.
func (i *Index) collectEntries(view *indexView, out *[]Entry) {
	var prefix [hashBytes]byte
	for index := 0; index < radixFanout; index++ {
		child := view.root[index]
		if child == 0 {
			continue
		}
		prefix[0] = byte(index)
		ref, isNode := decodeChild(child)
		if isNode {
			i.walkNode(view.arena, ref, 1, &prefix, out)
			continue
		}
		i.extractLeaf(view.arena, ref, 1, &prefix, out)
	}
}

// walkNode appends the entries below a node at level to out, tracking the
// consumed radix prefix so leaf tails can be reassembled into full hashes.
func (i *Index) walkNode(arena []byte, ref pagealloc.Ref, level int, prefix *[hashBytes]byte, out *[]Entry) {
	if level >= maxDepth || !validExtent(arena, ref, radixNodeBytes) {
		return
	}
	for index := 0; index < radixFanout; index++ {
		child := nodeChild(arena, ref, byte(index))
		if child == 0 {
			continue
		}
		prefix[level] = byte(index)
		childRef, childIsNode := decodeChild(child)
		if childIsNode {
			i.walkNode(arena, childRef, level+1, prefix, out)
			continue
		}
		i.extractLeaf(arena, childRef, level+1, prefix, out)
	}
}

// extractLeaf appends every entry of the leaf at depth, rebuilding each full
// hash from the consumed prefix and the stored tail.
func (i *Index) extractLeaf(arena []byte, ref pagealloc.Ref, depth int, prefix *[hashBytes]byte, out *[]Entry) {
	header, err := readLeafHeader(arena, ref)
	if err != nil || header.depth != depth {
		return
	}
	tailLen := tailLenFor(depth)
	offset := extentOffset(ref)
	for s := 0; s < header.count; s++ {
		var hash Hash
		copy(hash[:depth], prefix[:depth])
		copy(hash[depth:], arena[offset+uint64(leafHeaderSize+tailLen*s):])
		var id uint64
		if header.idBytes == id32Bytes {
			id = uint64(binary.LittleEndian.Uint32(arena[offset+uint64(header.idsOffset+4*s):]))
		} else {
			id = binary.LittleEndian.Uint64(arena[offset+uint64(header.idsOffset+8*s):])
		}
		*out = append(*out, Entry{Hash: hash, ImageID: id})
	}
}

// Sync flushes a file-backed arena to disk. It is a no-op for heap arenas.
func (i *Index) Sync() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	if i.sync == nil {
		return nil
	}
	return i.sync()
}

// Close marks the index closed and releases retired extents and any file-backed
// arena. Published views remain readable by concurrent readers during the call.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	err := i.alloc.ReclaimAll()
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
