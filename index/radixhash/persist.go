package radixhash

import (
	"fmt"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// NewMmap creates or truncates a file-backed arena of arenaBytes and returns an
// empty, check-pointable index over it. The file is sparse until written, so a
// large logical arena consumes disk and resident memory only for touched pages.
// The initial empty root is persisted immediately, so OpenMmap can reopen the
// file. Sync flushes; Close unmaps and closes.
func NewMmap(path string, arenaBytes uint64, config Config) (*Index, error) {
	mapped, err := storage.CreateMapped(path, int64(arenaBytes))
	if err != nil {
		return nil, err
	}
	index, err := newMappedIndex(mapped, config)
	if err != nil {
		_ = mapped.Close()
		return nil, err
	}
	if err := index.writeSuperblock(index.slot); err != nil {
		_ = mapped.Close()
		return nil, err
	}
	return index, nil
}

// OpenMmap opens an existing file-backed index, reconstructs its allocation
// state by walking the persisted tree, and returns it. The arena size is the
// file size; Config is runtime policy and is not persisted.
func OpenMmap(path string, config Config) (*Index, error) {
	mapped, err := storage.OpenMapped(path)
	if err != nil {
		return nil, err
	}
	index, err := newMappedIndex(mapped, config)
	if err != nil {
		_ = mapped.Close()
		return nil, err
	}
	if err := index.load(); err != nil {
		_ = mapped.Close()
		return nil, err
	}
	return index, nil
}

// newMappedIndex builds an index over a mapped arena and pins the fixed header.
func newMappedIndex(mapped *storage.MappedFile, config Config) (*Index, error) {
	allocator, err := pagealloc.New(uint64(len(mapped.Bytes())))
	if err != nil {
		return nil, err
	}
	if err := reserveHeader(allocator); err != nil {
		return nil, err
	}
	index, err := newIndex(allocator, mapped.Bytes()[:allocator.TotalBytes()], config)
	if err != nil {
		return nil, err
	}
	index.persistent = true
	index.release = mapped.Close
	index.sync = mapped.Sync
	return index, nil
}

// load validates the superblocks and root, reconstructs allocator state from
// the reachable tree, and publishes the recovered view. The mapped arena comes
// from the view newIndex already published, so reconstruction and the final
// view agree by construction.
func (i *Index) load() error {
	arena := i.view.Load().arena
	if uint64(len(arena)) < rootOffset(superblockSlots-1)+uint64(radixNodeBytes) {
		return fmt.Errorf("%w: arena too small for header", ErrCorrupt)
	}
	var (
		best superblock
		slot = -1
	)
	for candidate := 0; candidate < superblockSlots; candidate++ {
		offset := superblockOffset(candidate)
		if offset+uint64(superblockBytes) > uint64(len(arena)) {
			continue
		}
		root := arena[rootOffset(candidate) : rootOffset(candidate)+uint64(radixNodeBytes)]
		sb, ok := decodeRadixSuperblock(arena[offset:offset+uint64(superblockBytes)], root)
		if !ok {
			continue
		}
		if slot == -1 || sb.generation > best.generation {
			best, slot = sb, candidate
		}
	}
	if slot == -1 {
		return fmt.Errorf("%w: no valid superblock", ErrCorrupt)
	}

	var root [radixFanout]uint32
	if err := decodeRoot(arena[rootOffset(slot):rootOffset(slot)+uint64(radixNodeBytes)], &root); err != nil {
		return err
	}
	entries, buckets, depth, err := i.markReachable(arena, &root)
	if err != nil {
		return err
	}
	if entries != best.entries || buckets != best.rootBuckets || depth != best.depth {
		return fmt.Errorf("%w: superblock counts disagree with the tree", ErrCorrupt)
	}
	i.slot = slot
	i.view.Store(&indexView{
		generation:  best.generation,
		root:        &root,
		arena:       arena,
		entries:     entries,
		rootBuckets: buckets,
		depth:       depth,
	})
	return nil
}

// markReachable reserves every extent reachable from root and recomputes the
// entry count, root-bucket count, and maximum leaf depth. This is how reopen
// reclaims space that was retired before the previous checkpoint.
func (i *Index) markReachable(arena []byte, root *[radixFanout]uint32) (entries, buckets uint64, depth uint8, err error) {
	for index := 0; index < radixFanout; index++ {
		child := root[index]
		if child == 0 {
			continue
		}
		buckets++
		ref, isNode := decodeChild(child)
		if isNode {
			err = i.markNode(arena, ref, 1, &entries, &depth)
		} else {
			err = i.markLeaf(arena, ref, 1, &entries, &depth)
		}
		if err != nil {
			return 0, 0, 0, err
		}
	}
	return entries, buckets, depth, nil
}

func (i *Index) markNode(arena []byte, ref pagealloc.Ref, level int, entries *uint64, depth *uint8) error {
	if level >= maxDepth || !validExtent(arena, ref, radixNodeBytes) {
		return fmt.Errorf("%w: node outside arena", ErrCorrupt)
	}
	if err := i.alloc.Reserve(ref, radixNodeBytes); err != nil {
		return err
	}
	for index := 0; index < radixFanout; index++ {
		child := nodeChild(arena, ref, byte(index))
		if child == 0 {
			continue
		}
		childRef, isNode := decodeChild(child)
		var err error
		if isNode {
			err = i.markNode(arena, childRef, level+1, entries, depth)
		} else {
			err = i.markLeaf(arena, childRef, level+1, entries, depth)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Index) markLeaf(arena []byte, ref pagealloc.Ref, leafDepth int, entries *uint64, depth *uint8) error {
	header, err := readLeafHeader(arena, ref)
	if err != nil {
		return err
	}
	if header.depth != leafDepth {
		return fmt.Errorf("%w: leaf depth %d at level %d", ErrCorrupt, header.depth, leafDepth)
	}
	if err := i.alloc.Reserve(ref, header.extentBytes); err != nil {
		return err
	}
	*entries += uint64(header.count)
	if uint8(leafDepth) > *depth {
		*depth = uint8(leafDepth)
	}
	return nil
}

// writeSuperblock serializes the current root into a slot and flushes it. The
// slot is not marked active until the caller publishes it.
func (i *Index) writeSuperblock(slot int) error {
	if !i.persistent {
		return nil
	}
	view := i.view.Load()
	arena := view.arena
	root := arena[rootOffset(slot) : rootOffset(slot)+uint64(radixNodeBytes)]
	encodeRoot(root, view.root)
	offset := superblockOffset(slot)
	if offset+uint64(superblockBytes) > uint64(len(arena)) {
		return fmt.Errorf("%w: superblock outside arena", ErrCorrupt)
	}
	encodeRadixSuperblock(arena[offset:offset+uint64(superblockBytes)], superblock{
		generation:  view.generation,
		entries:     view.entries,
		rootBuckets: view.rootBuckets,
		depth:       view.depth,
	}, root)
	if i.sync != nil {
		return i.sync()
	}
	return nil
}

// Checkpoint persists the current root and metadata into the inactive
// superblock slot and flushes. It is a no-op for heap arenas. Call it before
// Close to guarantee the latest generation is durable.
func (i *Index) Checkpoint() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if !i.persistent {
		return nil
	}
	next := 1 - i.slot
	if err := i.writeSuperblock(next); err != nil {
		return err
	}
	i.slot = next
	return nil
}
