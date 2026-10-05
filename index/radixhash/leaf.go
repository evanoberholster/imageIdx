package radixhash

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	// leafMagic identifies a leaf extent.
	leafMagic uint32 = 0x524c4846 // "RLHF"
	// leafVersion is the current leaf format version.
	leafVersion uint16 = 1
	// leafHeaderSize is the fixed header at the start of every leaf.
	leafHeaderSize = 32
	// leafFlagID32 marks a leaf whose ImageIDs are stored as 32-bit values.
	leafFlagID32 uint8 = 1 << 0
	// id32Bytes and id64Bytes are the two supported id widths.
	id32Bytes = 4
	id64Bytes = 8
)

// slot is one decoded leaf record. Only tail[:tailLen] is meaningful, where
// tailLen = hashBytes - depth for the owning leaf.
type slot struct {
	tail [hashBytes]byte
	id   uint64
}

// leafHeader is the validated fixed header of a leaf extent. depth is the
// number of radix bytes already consumed by the path; the leaf stores
// hash[depth:32] for each slot. idBytes is 4 or 8 depending on whether every
// id in the leaf fits in 32 bits.
type leafHeader struct {
	count       int
	capacity    int
	depth       int
	idBytes     int
	extentBytes uint32
	tailOffset  int
	idsOffset   int
}

func align8(value int) int { return (value + 7) &^ 7 }

// tailLenFor returns the number of unconsumed hash bytes carried by a leaf at
// the given radix depth.
func tailLenFor(depth int) int { return hashBytes - depth }

// idBytesForSlots picks the narrowest id width that holds every id.
func idBytesForSlots(slots []slot) int {
	for i := range slots {
		if slots[i].id > math.MaxUint32 {
			return id64Bytes
		}
	}
	return id32Bytes
}

func idsOffsetFor(tailLen, capacity int) int {
	return align8(leafHeaderSize + tailLen*capacity)
}

// sizeForCapacity returns the bytes needed to store capacity slots at depth and
// the given id width.
func sizeForCapacity(depth, idBytes, capacity int) int {
	return idsOffsetFor(tailLenFor(depth), capacity) + idBytes*capacity
}

// capacityForSize returns the largest slot count that fits in extentBytes for a
// leaf at depth and id width.
func capacityForSize(depth, idBytes int, extentBytes uint32) int {
	size := int(extentBytes)
	if size < leafHeaderSize {
		return 0
	}
	stride := tailLenFor(depth) + idBytes
	if stride < 1 {
		stride = 1
	}
	capacity := size/stride + 1
	for capacity > 0 && sizeForCapacity(depth, idBytes, capacity) > size {
		capacity--
	}
	for sizeForCapacity(depth, idBytes, capacity+1) <= size {
		capacity++
	}
	return capacity
}

// encodeLeaf writes slots into a leaf extent at the given depth. The extent
// must be exactly one pagealloc extent and may be larger than the payload.
func encodeLeaf(extent []byte, depth int, slots []slot) error {
	if depth < 1 || depth > maxDepth {
		return fmt.Errorf("%w: invalid leaf depth %d", ErrCorrupt, depth)
	}
	idBytes := idBytesForSlots(slots)
	capacity := capacityForSize(depth, idBytes, uint32(len(extent)))
	if capacity < 1 || len(slots) > capacity {
		return fmt.Errorf("%w: %d slots do not fit in %d bytes at depth %d", ErrCorrupt, len(slots), len(extent), depth)
	}
	tailLen := tailLenFor(depth)
	idsOffset := idsOffsetFor(tailLen, capacity)
	binary.LittleEndian.PutUint32(extent[0:4], leafMagic)
	binary.LittleEndian.PutUint16(extent[4:6], leafVersion)
	extent[6] = byte(depth)
	extent[7] = 0
	if idBytes == id32Bytes {
		extent[7] = leafFlagID32
	}
	binary.LittleEndian.PutUint32(extent[8:12], uint32(len(slots)))
	binary.LittleEndian.PutUint32(extent[12:16], uint32(capacity))
	binary.LittleEndian.PutUint32(extent[16:20], uint32(leafHeaderSize))
	binary.LittleEndian.PutUint32(extent[20:24], uint32(idsOffset))
	binary.LittleEndian.PutUint32(extent[24:28], uint32(len(extent)))
	binary.LittleEndian.PutUint32(extent[28:32], 0)
	for i := range slots {
		copy(extent[leafHeaderSize+tailLen*i:], slots[i].tail[:tailLen])
		if idBytes == id32Bytes {
			binary.LittleEndian.PutUint32(extent[idsOffset+4*i:], uint32(slots[i].id))
		} else {
			binary.LittleEndian.PutUint64(extent[idsOffset+8*i:], slots[i].id)
		}
	}
	return nil
}

// readLeafHeader validates and decodes a leaf header. Array offsets are
// recomputed from depth, id width, and capacity and must match what is stored,
// so a corrupted header cannot steer reads outside the extent.
func readLeafHeader(arena []byte, ref pagealloc.Ref) (leafHeader, error) {
	if !validExtent(arena, ref, leafHeaderSize) {
		return leafHeader{}, fmt.Errorf("%w: header outside arena", ErrCorrupt)
	}
	offset := extentOffset(ref)
	region := arena[offset:]
	if binary.LittleEndian.Uint32(region[0:4]) != leafMagic {
		return leafHeader{}, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if binary.LittleEndian.Uint16(region[4:6]) != leafVersion {
		return leafHeader{}, fmt.Errorf("%w: bad version", ErrCorrupt)
	}
	depth := int(region[6])
	if depth < 1 || depth > maxDepth {
		return leafHeader{}, fmt.Errorf("%w: bad depth %d", ErrCorrupt, depth)
	}
	idBytes := id64Bytes
	if region[7]&leafFlagID32 != 0 {
		idBytes = id32Bytes
	}
	count := int(binary.LittleEndian.Uint32(region[8:12]))
	capacity := int(binary.LittleEndian.Uint32(region[12:16]))
	if capacity < 1 || count < 1 || count > capacity {
		return leafHeader{}, fmt.Errorf("%w: count %d capacity %d", ErrCorrupt, count, capacity)
	}
	tailOffset := int(binary.LittleEndian.Uint32(region[16:20]))
	idsOffset := int(binary.LittleEndian.Uint32(region[20:24]))
	extentBytes := binary.LittleEndian.Uint32(region[24:28])
	if tailOffset != leafHeaderSize || idsOffset != idsOffsetFor(tailLenFor(depth), capacity) {
		return leafHeader{}, fmt.Errorf("%w: inconsistent offsets", ErrCorrupt)
	}
	if extentBytes < uint32(leafHeaderSize) || capacityForSize(depth, idBytes, extentBytes) != capacity {
		return leafHeader{}, fmt.Errorf("%w: inconsistent extent size %d", ErrCorrupt, extentBytes)
	}
	if !validExtent(arena, ref, extentBytes) {
		return leafHeader{}, fmt.Errorf("%w: extent outside arena", ErrCorrupt)
	}
	return leafHeader{
		count:       count,
		capacity:    capacity,
		depth:       depth,
		idBytes:     idBytes,
		extentBytes: extentBytes,
		tailOffset:  tailOffset,
		idsOffset:   idsOffset,
	}, nil
}

// readLeafSlotsInto decodes every slot of a leaf into dst, reusing its backing
// array when it is large enough; the returned slice may be dst resliced or a
// fresh allocation. header carries the depth, id width, and extent size needed
// to interpret and retire the leaf.
func readLeafSlotsInto(dst []slot, arena []byte, ref pagealloc.Ref) ([]slot, leafHeader, error) {
	header, err := readLeafHeader(arena, ref)
	if err != nil {
		return dst[:0], leafHeader{}, err
	}
	tailLen := tailLenFor(header.depth)
	offset := extentOffset(ref)
	if cap(dst) < header.count {
		dst = make([]slot, header.count)
	} else {
		dst = dst[:header.count]
	}
	for i := range dst {
		copy(dst[i].tail[:tailLen], arena[offset+uint64(leafHeaderSize+tailLen*i):])
		if header.idBytes == id32Bytes {
			dst[i].id = uint64(binary.LittleEndian.Uint32(arena[offset+uint64(header.idsOffset+4*i):]))
		} else {
			dst[i].id = binary.LittleEndian.Uint64(arena[offset+uint64(header.idsOffset+8*i):])
		}
	}
	return dst, header, nil
}

// parseLeafHeader reads a leaf header on the hot lookup path. It validates the
// depth, id width, offsets, and that the arrays fit the extent, but unlike
// readLeafHeader it does not recompute the maximal slot capacity (a loop).
// Leaves are fully validated when written and by Verify, so the read path
// trusts the stored capacity while still guarding every array bound.
func parseLeafHeader(arena []byte, ref pagealloc.Ref) (leafHeader, error) {
	if !validExtent(arena, ref, leafHeaderSize) {
		return leafHeader{}, fmt.Errorf("%w: header outside arena", ErrCorrupt)
	}
	region := arena[extentOffset(ref):]
	depth := int(region[6])
	if depth < 1 || depth > maxDepth {
		return leafHeader{}, fmt.Errorf("%w: bad depth %d", ErrCorrupt, depth)
	}
	idBytes := id64Bytes
	if region[7]&leafFlagID32 != 0 {
		idBytes = id32Bytes
	}
	count := int(binary.LittleEndian.Uint32(region[8:12]))
	capacity := int(binary.LittleEndian.Uint32(region[12:16]))
	if capacity < 1 || count < 1 || count > capacity {
		return leafHeader{}, fmt.Errorf("%w: count %d capacity %d", ErrCorrupt, count, capacity)
	}
	tailLen := tailLenFor(depth)
	idsOffset := int(binary.LittleEndian.Uint32(region[20:24]))
	extentBytes := binary.LittleEndian.Uint32(region[24:28])
	if idsOffset != idsOffsetFor(tailLen, capacity) {
		return leafHeader{}, fmt.Errorf("%w: inconsistent offsets", ErrCorrupt)
	}
	if sizeForCapacity(depth, idBytes, capacity) > int(extentBytes) || !validExtent(arena, ref, extentBytes) {
		return leafHeader{}, fmt.Errorf("%w: extent size %d", ErrCorrupt, extentBytes)
	}
	return leafHeader{
		count:       count,
		capacity:    capacity,
		depth:       depth,
		idBytes:     idBytes,
		extentBytes: extentBytes,
		tailOffset:  leafHeaderSize,
		idsOffset:   idsOffset,
	}, nil
}

// scanLeaf invokes visit for every slot whose tail equals query[depth:32]. It
// always binary-searches the sorted tail array.
//
// BenchmarkLeafSearchCrossover shows binary search beats the SIMD linear scan at
// every occupancy up to the kernel's 64-slot limit, and the linear path also
// allocates because the prefix is passed through an indirect kernel call, which
// violates the allocation-free lookup requirement. The SIMD scan is kept only as
// a benchmark reference (scanLeafLinear in benchmark_test.go) and is not on the
// hot path.
func scanLeaf(arena []byte, ref pagealloc.Ref, query Hash, depth int, visit func(ImageID) bool) error {
	header, err := parseLeafHeader(arena, ref)
	if err != nil {
		return err
	}
	if header.depth != depth {
		return fmt.Errorf("%w: leaf depth %d reached at depth %d", ErrCorrupt, header.depth, depth)
	}
	return scanLeafBinary(arena, ref, query, depth, header, visit)
}

// scanLeafBinary lower-bounds the sorted tail array and walks the equal run,
// reading ids through zero-copy typed views. It is the correctness reference
// for every leaf size.
func scanLeafBinary(arena []byte, ref pagealloc.Ref, query Hash, depth int, header leafHeader, visit func(ImageID) bool) error {
	tailLen := tailLenFor(depth)
	want := query[depth:]
	base := extentOffset(ref) + uint64(leafHeaderSize)
	lo, hi := 0, header.count
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		tail := arena[base+uint64(tailLen*mid):][:tailLen]
		if bytes.Compare(tail, want) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}

	idsBase := extentOffset(ref) + uint64(header.idsOffset)
	if header.idBytes == id32Bytes {
		ids, err := storage.View[uint32](arena, idsBase, uint64(header.count), ErrCorrupt, ErrCorrupt)
		if err != nil {
			return err
		}
		for i := lo; i < header.count; i++ {
			if bytes.Compare(arena[base+uint64(tailLen*i):][:tailLen], want) != 0 {
				break
			}
			if !visit(ImageID(ids[i])) {
				return nil
			}
		}
		return nil
	}
	ids, err := storage.View[uint64](arena, idsBase, uint64(header.count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return err
	}
	for i := lo; i < header.count; i++ {
		if bytes.Compare(arena[base+uint64(tailLen*i):][:tailLen], want) != 0 {
			break
		}
		if !visit(ImageID(ids[i])) {
			return nil
		}
	}
	return nil
}

// partitionScratch holds the reusable writer buffers for one rebuild: the
// per-level 256-way partition buffers and the leaf read/merge temporaries.
// Buffers are indexed by node level so a parent's grouped records stay valid
// while its children recurse. Writes are serialized, so one scratch per writer
// (or per worker in the large-batch fan-out) is safe.
type partitionScratch struct {
	levels   [maxDepth][]slot
	existing []slot
	merged   []slot
}

// buffer returns a length-n slice for the given level, reusing the existing
// backing array when it is large enough.
func (s *partitionScratch) buffer(level, n int) []slot {
	if level < 0 {
		level = 0
	} else if level >= maxDepth {
		level = maxDepth - 1
	}
	if cap(s.levels[level]) < n {
		s.levels[level] = make([]slot, n)
	}
	return s.levels[level][:n]
}

// partitionSlots groups slots by their next unconsumed byte and shifts each
// child tail down by one byte into dst. It uses fixed-size counting/offset
// arrays and one contiguous buffer instead of a map and per-child slices.
//
// The partition is stable, so records within a key keep their input order and
// leaf construction and merging need no second sort. After the call, key's
// records are dst[offsets[key]:offsets[key+1]] and counts[key] is their count.
// dst must have len(slots).
func partitionSlots(dst, slots []slot, tailLen int, counts *[radixFanout]uint32, offsets *[radixFanout + 1]uint32) {
	*counts = [radixFanout]uint32{}
	for i := range slots {
		counts[slots[i].tail[0]]++
	}
	var sum uint32
	for key := 0; key < radixFanout; key++ {
		offsets[key] = sum
		sum += counts[key]
	}
	offsets[radixFanout] = sum
	var cursor [radixFanout]uint32
	copy(cursor[:], offsets[:radixFanout])
	for i := range slots {
		current := &slots[i]
		key := current.tail[0]
		position := cursor[key]
		cursor[key]++
		var child slot
		child.id = current.id
		copy(child.tail[:], current.tail[1:tailLen])
		dst[position] = child
	}
}

// compareSlot orders by tail then ImageID, returning -1, 0, or 1.
func compareSlot(a, b *slot, tailLen int) int {
	if comparison := bytes.Compare(a.tail[:tailLen], b.tail[:tailLen]); comparison != 0 {
		return comparison
	}
	switch {
	case a.id < b.id:
		return -1
	case a.id > b.id:
		return 1
	default:
		return 0
	}
}

// lessSlot orders by tail then ImageID.
func lessSlot(a, b *slot, tailLen int) bool {
	return compareSlot(a, b, tailLen) < 0
}

// sameSlot reports whether two slots are the same (tail, ImageID) pair.
func sameSlot(a, b *slot, tailLen int) bool {
	return a.id == b.id && bytes.Equal(a.tail[:tailLen], b.tail[:tailLen])
}

// sortSlots orders slots by (tail, ImageID). Small leaves use insertion sort to
// avoid the sort.Slice closure; larger leaves use a comparison sort.
func sortSlots(slots []slot, tailLen int) {
	if len(slots) <= 16 {
		for i := 1; i < len(slots); i++ {
			current := slots[i]
			j := i - 1
			for j >= 0 && lessSlot(&current, &slots[j], tailLen) {
				slots[j+1] = slots[j]
				j--
			}
			slots[j+1] = current
		}
		return
	}
	sort.Slice(slots, func(i, j int) bool { return lessSlot(&slots[i], &slots[j], tailLen) })
}

// dedupeSlots sorts slots in place and removes exact (tail, ImageID) duplicates.
func dedupeSlots(slots []slot, tailLen int) []slot {
	if len(slots) < 2 {
		return slots
	}
	sortSlots(slots, tailLen)
	unique := slots[:1]
	for i := 1; i < len(slots); i++ {
		last := &unique[len(unique)-1]
		if slots[i].id == last.id && bytes.Equal(slots[i].tail[:tailLen], last.tail[:tailLen]) {
			continue
		}
		unique = append(unique, slots[i])
	}
	return unique
}

// mergeSlotsInto returns the sorted, deduplicated union of existing (already
// sorted) and added, writing into dst and reusing its backing array when it is
// large enough. dst must not alias existing or added. Only the small incoming
// group is sorted; the two sorted runs are then merged linearly, so a small
// update never re-sorts the existing leaf. The result is sorted by
// (tail, ImageID) with exact duplicates collapsed.
func mergeSlotsInto(dst, existing, added []slot, tailLen int) []slot {
	if len(added) == 0 {
		if cap(dst) < len(existing) {
			dst = make([]slot, 0, len(existing))
		}
		return append(dst[:0], existing...)
	}
	sortSlots(added, tailLen)
	if cap(dst) < len(existing)+len(added) {
		dst = make([]slot, 0, len(existing)+len(added))
	}
	dst = dst[:0]
	i, j := 0, 0
	for i < len(existing) || j < len(added) {
		var next slot
		switch {
		case j >= len(added) || (i < len(existing) && compareSlot(&existing[i], &added[j], tailLen) <= 0):
			next = existing[i]
			i++
		default:
			next = added[j]
			j++
		}
		if len(dst) > 0 && sameSlot(&dst[len(dst)-1], &next, tailLen) {
			continue
		}
		dst = append(dst, next)
	}
	return dst
}
