package radixhash

import (
	"encoding/binary"
	"fmt"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// Child reference encoding. A pagealloc Ref is a 512-byte block address, so a
// node stores it shifted left by one bit with the low bit tagging the target as
// a leaf or another radix node. Zero means the child does not exist.
const (
	childKindLeaf uint32 = 0
	childKindNode uint32 = 1
)

// encodeChild packs a pagealloc reference and its kind into a node slot.
func encodeChild(ref pagealloc.Ref, node bool) uint32 {
	encoded := uint32(ref) << 1
	if node {
		encoded |= childKindNode
	}
	return encoded
}

// decodeChild unpacks a node slot. The boolean reports whether the target is a
// radix node rather than a leaf.
func decodeChild(encoded uint32) (pagealloc.Ref, bool) {
	return pagealloc.Ref(encoded >> 1), encoded&childKindNode != 0
}

// extentOffset is the arena byte offset of a pagealloc reference.
func extentOffset(ref pagealloc.Ref) uint64 {
	return uint64(ref) * uint64(pagealloc.MinBlockSize)
}

// validExtent reports whether an extent of size bytes at ref lies inside the
// arena. It guards node reads, which carry no header of their own.
func validExtent(arena []byte, ref pagealloc.Ref, size uint32) bool {
	if ref == 0 {
		return false
	}
	offset := extentOffset(ref)
	return offset+uint64(size) <= uint64(len(arena))
}

// readNode copies all 256 children of the node at ref into children.
func readNode(arena []byte, ref pagealloc.Ref, children *[radixFanout]uint32) {
	offset := extentOffset(ref)
	for i := 0; i < radixFanout; i++ {
		children[i] = binary.LittleEndian.Uint32(arena[offset+uint64(i)*4:])
	}
}

// nodeChild returns one child slot of the node at ref.
func nodeChild(arena []byte, ref pagealloc.Ref, index byte) uint32 {
	offset := extentOffset(ref) + uint64(index)*4
	return binary.LittleEndian.Uint32(arena[offset:])
}

// writeNode stores all 256 children of a node at ref.
func writeNode(arena []byte, ref pagealloc.Ref, children *[radixFanout]uint32) error {
	if !validExtent(arena, ref, radixNodeBytes) {
		return fmt.Errorf("%w: node extent outside arena", ErrCorrupt)
	}
	offset := extentOffset(ref)
	for i := 0; i < radixFanout; i++ {
		binary.LittleEndian.PutUint32(arena[offset+uint64(i)*4:], children[i])
	}
	return nil
}
