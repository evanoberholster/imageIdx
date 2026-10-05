package radixhash

import (
	"encoding/binary"
	"fmt"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// compactBatchEntries bounds how many entries the streaming walker hands to the
// writer at once. It is a var so benchmarks can sweep the batch size.
var compactBatchEntries = 1 << 14

// entryWalker iterates one published view in radix/hash order and fills a
// caller-owned buffer with bounded batches. It never allocates per entry: the
// frame stack is bounded by the maximum radix depth and is reused across calls,
// and the radix prefix is a fixed array. The view it walks is immutable, so a
// rebuild can stream from it while the writer publishes a replacement.
//
// Next reports corruption instead of skipping it. Compaction must never
// publish a rebuild that silently dropped records, so an invalid node or leaf
// stops the walk with ErrCorrupt and Compact aborts without publishing.
type entryWalker struct {
	arena  []byte
	root   *[radixFanout]uint32
	stack  []walkFrame
	prefix [hashBytes]byte
}

// walkFrame is one node or leaf on the current DFS path.
type walkFrame struct {
	ref    pagealloc.Ref
	root   bool // virtual root frame over the view's root array
	leaf   bool
	level  int // node level; leaf depth for leaves
	index  int // next child (node) or slot (leaf)
	header leafHeader
}

// newEntryWalker returns a walker positioned before the first entry of view.
func newEntryWalker(view *indexView) *entryWalker {
	w := &entryWalker{arena: view.arena, root: view.root}
	w.stack = append(w.stack, walkFrame{root: true, level: 0})
	return w
}

// Next fills dst (up to len(dst)) with entries in radix/hash order and returns
// the number written, whether any entries remain, and the first corruption
// encountered. dst must be a full-capacity slice (len == cap); Next writes only
// the first n returned elements. On corruption n is the count of valid entries
// produced before the fault and more is meaningless.
func (w *entryWalker) Next(dst []Entry) (n int, more bool, err error) {
	written := 0
	for written < len(dst) && len(w.stack) != 0 {
		topIndex := len(w.stack) - 1
		top := &w.stack[topIndex]
		if top.leaf {
			count, leafErr := w.drainLeaf(top, dst[written:])
			written += count
			if leafErr != nil {
				return written, false, leafErr
			}
			if top.index >= top.header.count {
				w.stack = w.stack[:topIndex]
			}
			continue
		}
		if top.index >= radixFanout {
			w.stack = w.stack[:topIndex]
			continue
		}
		childIndex := top.index
		top.index++
		level := top.level
		ref := top.ref
		isRoot := top.root
		var child uint32
		if isRoot {
			child = w.root[childIndex]
		} else {
			if !validExtent(w.arena, ref, radixNodeBytes) {
				return written, false, fmt.Errorf("%w: node extent outside arena at level %d", ErrCorrupt, level)
			}
			child = nodeChild(w.arena, ref, byte(childIndex))
		}
		if child == 0 {
			continue
		}
		w.prefix[level] = byte(childIndex)
		childRef, isNode := decodeChild(child)
		if isNode {
			w.stack = append(w.stack, walkFrame{ref: childRef, level: level + 1})
			continue
		}
		header, headerErr := readLeafHeader(w.arena, childRef)
		if headerErr != nil {
			return written, false, fmt.Errorf("%w: leaf at level %d: %v", ErrCorrupt, level+1, headerErr)
		}
		if header.depth != level+1 {
			return written, false, fmt.Errorf("%w: leaf depth %d reached at level %d", ErrCorrupt, header.depth, level+1)
		}
		if header.idBytes != id32Bytes && header.idBytes != id64Bytes {
			return written, false, fmt.Errorf("%w: leaf id width %d", ErrCorrupt, header.idBytes)
		}
		w.stack = append(w.stack, walkFrame{ref: childRef, leaf: true, level: level + 1, header: header})
	}
	return written, len(w.stack) != 0, nil
}

// drainLeaf appends up to len(dst) remaining entries from the top leaf. It
// re-checks the extent bound so a header that survived validation cannot steer
// a read outside the arena.
func (w *entryWalker) drainLeaf(top *walkFrame, dst []Entry) (int, error) {
	depth := top.level
	tailLen := tailLenFor(depth)
	offset := extentOffset(top.ref)
	if !validExtent(w.arena, top.ref, top.header.extentBytes) {
		return 0, fmt.Errorf("%w: leaf extent outside arena at depth %d", ErrCorrupt, depth)
	}
	written := 0
	for written < len(dst) && top.index < top.header.count {
		slot := top.index
		tailStart := offset + uint64(leafHeaderSize+tailLen*slot)
		idsStart := offset + uint64(top.header.idsOffset)
		var entry Entry
		copy(entry.Hash[:depth], w.prefix[:depth])
		if tailStart+uint64(tailLen) > uint64(len(w.arena)) {
			return written, fmt.Errorf("%w: leaf tail out of bounds", ErrCorrupt)
		}
		copy(entry.Hash[depth:], w.arena[tailStart:tailStart+uint64(tailLen)])
		if top.header.idBytes == id32Bytes {
			if idsStart+uint64(4*slot)+4 > uint64(len(w.arena)) {
				return written, fmt.Errorf("%w: leaf id out of bounds", ErrCorrupt)
			}
			entry.ImageID = uint64(binary.LittleEndian.Uint32(w.arena[idsStart+uint64(4*slot):]))
		} else {
			if idsStart+uint64(8*slot)+8 > uint64(len(w.arena)) {
				return written, fmt.Errorf("%w: leaf id out of bounds", ErrCorrupt)
			}
			entry.ImageID = binary.LittleEndian.Uint64(w.arena[idsStart+uint64(8*slot):])
		}
		dst[written] = entry
		written++
		top.index++
	}
	return written, nil
}
