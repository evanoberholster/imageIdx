package radixhash

import (
	"encoding/binary"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// compactBatchEntries bounds how many entries the streaming walker hands to the
// writer at once. It is a var so benchmarks can sweep the batch size.
var compactBatchEntries = 1 << 16

// entryWalker iterates one published view in radix/hash order and fills a
// caller-owned buffer with bounded batches. It never allocates per entry: the
// frame stack is bounded by the maximum radix depth and is reused across calls,
// and the radix prefix is a fixed array. The view it walks is immutable, so a
// rebuild can stream from it while the writer publishes a replacement.
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

// next fills dst (up to cap(dst)) with entries in radix/hash order and returns
// the number written and whether any entries remain. dst must be a full-capacity
// slice (len == cap); next writes only the first n returned elements.
func (w *entryWalker) next(dst []Entry) (int, bool) {
	written := 0
	for written < len(dst) && len(w.stack) != 0 {
		topIndex := len(w.stack) - 1
		top := &w.stack[topIndex]
		if top.leaf {
			written += w.drainLeaf(top, dst[written:])
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
		switch {
		case isRoot:
			child = w.root[childIndex]
		case validExtent(w.arena, ref, radixNodeBytes):
			child = nodeChild(w.arena, ref, byte(childIndex))
		default:
			w.stack = w.stack[:topIndex]
			continue
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
		header, err := readLeafHeader(w.arena, childRef)
		if err != nil || header.depth != level+1 {
			continue
		}
		w.stack = append(w.stack, walkFrame{ref: childRef, leaf: true, level: level + 1, header: header})
	}
	return written, len(w.stack) != 0
}

// drainLeaf appends up to len(dst) remaining entries from the top leaf.
func (w *entryWalker) drainLeaf(top *walkFrame, dst []Entry) int {
	depth := top.level
	tailLen := tailLenFor(depth)
	offset := extentOffset(top.ref)
	written := 0
	for written < len(dst) && top.index < top.header.count {
		slot := top.index
		var entry Entry
		copy(entry.Hash[:depth], w.prefix[:depth])
		copy(entry.Hash[depth:], w.arena[offset+uint64(leafHeaderSize+tailLen*slot):])
		if top.header.idBytes == id32Bytes {
			entry.ImageID = uint64(binary.LittleEndian.Uint32(w.arena[offset+uint64(top.header.idsOffset+4*slot):]))
		} else {
			entry.ImageID = binary.LittleEndian.Uint64(w.arena[offset+uint64(top.header.idsOffset+8*slot):])
		}
		dst[written] = entry
		written++
		top.index++
	}
	return written
}
