package hashidx

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
)

const (
	// HashSize is the fixed digest width in bytes.
	HashSize = 32
	// RecordSize is the fixed persistent record width: Hash32 + ImageID64.
	RecordSize = 40

	blockHeaderSize        = 32
	blockMagic      uint32 = 0x48424c4b // HBLK
	blockVersion    uint16 = 1

	// dirEntrySize is the fixed directory descriptor width:
	// FirstHash[32] + Offset u64 + Count u32 + Reserved u32.
	dirEntrySize = 48
)

func maxRecordsPerBlock(blockSize uint32) int {
	if blockSize <= blockHeaderSize {
		return 0
	}
	return int((blockSize - blockHeaderSize) / RecordSize)
}

func targetRecordsPerBlock(blockSize uint32, fillPct uint8) int {
	max := maxRecordsPerBlock(blockSize)
	target := max * int(fillPct) / 100
	if target < 1 {
		target = 1
	}
	if target > max {
		target = max
	}
	return target
}

// encodeBlockHeader writes a 32-byte header into dst.
func encodeBlockHeader(dst []byte, count uint32, crc uint32) {
	binary.LittleEndian.PutUint32(dst[0:4], blockMagic)
	binary.LittleEndian.PutUint16(dst[4:6], blockVersion)
	binary.LittleEndian.PutUint16(dst[6:8], 0)
	binary.LittleEndian.PutUint32(dst[8:12], count)
	binary.LittleEndian.PutUint32(dst[12:16], uint32(count)*RecordSize)
	binary.LittleEndian.PutUint32(dst[16:20], crc)
	clear(dst[20:32])
}

// decodeBlockHeader validates a block image and returns its record count.
func decodeBlockHeader(data []byte) (uint32, error) {
	if len(data) < blockHeaderSize {
		return 0, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(data[0:4]) != blockMagic {
		return 0, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(data[4:6]) != blockVersion {
		return 0, ErrUnsupported
	}
	count := binary.LittleEndian.Uint32(data[8:12])
	payloadLen := binary.LittleEndian.Uint32(data[12:16])
	if payloadLen != count*RecordSize {
		return 0, ErrCorrupt
	}
	if uint64(len(data))-blockHeaderSize < uint64(payloadLen) {
		return 0, ErrCorrupt
	}
	payload := data[blockHeaderSize : blockHeaderSize+payloadLen]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[16:20]) {
		return 0, ErrCorrupt
	}
	if !recordsSorted(payload) {
		return 0, ErrCorrupt
	}
	return count, nil
}

// blockLowerBound returns the index of the first record with hash >= query.
func blockLowerBound(records []byte, query Hash) int {
	n := len(records) / RecordSize
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) >> 1
		if bytes.Compare(records[mid*RecordSize:mid*RecordSize+32], query[:]) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// partitionEntries splits sorted unique entries into block-sized groups
// targeting target records each. It avoids splitting equal-hash runs when
// practical but always splits runs that exceed maxRecords so no block
// overflows.
func partitionEntries(entries []ExactEntry, maxRecords, target int) [][]ExactEntry {
	if len(entries) == 0 {
		return nil
	}
	if maxRecords < 1 {
		maxRecords = len(entries)
	}
	if target < 1 || target > maxRecords {
		target = maxRecords
	}
	var parts [][]ExactEntry
	start := 0
	for start < len(entries) {
		// Tentative end at target fullness, clamped to max and remainder.
		end := start + target
		if end > len(entries) {
			end = len(entries)
		}
		remaining := len(entries) - end
		// Avoid a tiny trailing block: if the remainder would be small,
		// absorb it unless that would overflow maxRecords.
		if remaining > 0 && remaining < target/2 && end-start+remaining <= maxRecords {
			end = len(entries)
			parts = append(parts, entries[start:end])
			break
		}
		if end < len(entries) {
			if end-start >= maxRecords {
				end = start + maxRecords
			} else {
				// Prefer a boundary between distinct hashes.
				boundary := end
				for boundary > start && entries[boundary].Hash == entries[boundary-1].Hash {
					boundary--
				}
				if boundary > start {
					// Only move the boundary back if the left part stays
					// reasonably full (at least half target); otherwise
					// keep the target boundary even if it splits a run.
					if boundary-start >= target/2 {
						end = boundary
					}
				} else {
					// The whole window is one hash run; extend forward to
					// the run end when it fits, else cut at maxRecords.
					runEnd := end
					for runEnd < len(entries) && entries[runEnd].Hash == entries[start].Hash {
						runEnd++
					}
					if runEnd-start <= maxRecords {
						end = runEnd
					} else {
						end = start + maxRecords
					}
				}
			}
		}
		// Hard clamp: never exceed maxRecords (a giant run spans blocks).
		if end-start > maxRecords {
			end = start + maxRecords
		}
		if end <= start {
			end = start + 1
			if end > len(entries) {
				end = len(entries)
			}
		}
		parts = append(parts, entries[start:end])
		start = end
	}
	return parts
}

// encodeRecord writes e into dst, which must have length >= RecordSize.
func encodeRecord(dst []byte, e ExactEntry) {
	copy(dst[:32], e.Hash[:])
	binary.LittleEndian.PutUint64(dst[32:40], uint64(e.ImageID))
}

// decodeRecord reads one record from src, which must have length >= RecordSize.
func decodeRecord(src []byte) ExactEntry {
	var e ExactEntry
	copy(e.Hash[:], src[:32])
	e.ImageID = ImageID(binary.LittleEndian.Uint64(src[32:40]))
	return e
}

// compareHash orders hashes lexicographically, matching bytes.Compare.
func compareHash(a, b Hash) int {
	return bytes.Compare(a[:], b[:])
}

// compareEntry orders by hash then image ID.
func compareEntry(a, b ExactEntry) int {
	if c := compareHash(a.Hash, b.Hash); c != 0 {
		return c
	}
	switch {
	case a.ImageID < b.ImageID:
		return -1
	case a.ImageID > b.ImageID:
		return 1
	default:
		return 0
	}
}

// sortEntries sorts entries by (Hash, ImageID) without the sort.Slice
// interface overhead.
func sortEntries(entries []ExactEntry) {
	slices.SortFunc(entries, compareEntry)
}

// recordsSorted validates that records (a Count*RecordSize byte slice) are
// sorted by (Hash, ImageID) with no duplicate pair.
func recordsSorted(records []byte) bool {
	if len(records)%RecordSize != 0 {
		return false
	}
	n := len(records) / RecordSize
	for i := 1; i < n; i++ {
		prev := records[(i-1)*RecordSize : i*RecordSize]
		cur := records[i*RecordSize : (i+1)*RecordSize]
		c := bytes.Compare(prev[:32], cur[:32])
		if c > 0 {
			return false
		}
		if c == 0 {
			prevID := binary.LittleEndian.Uint64(prev[32:40])
			curID := binary.LittleEndian.Uint64(cur[32:40])
			if prevID >= curID {
				return false
			}
		}
	}
	return true
}
