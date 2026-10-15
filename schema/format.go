package schema

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Format version 3: two 512-byte superblocks, followed by append-only
// checksummed directories and 64-byte metadata slots. Integers are LE.
// A directory entry is id:u16, nameLen:u16, block:u64, slotsOffset:u64,
// name:nameLen. A directory starts with count:u16 and ends in CRC32C:u32.
// Superblock payload: directory offset:u64, length:u64, CRC32C:u32,
// entity count:u16, reserved:u16, end of committed file:u64,
// definitions offset:u64, length:u64, CRC32C:u32, state:u8 (0 ACTIVE,
// 1 PREPARED), legacy copies retired:u8, reserved:10 bytes. Version 2 has 12 zero reserved bytes and
// is read as ACTIVE. A PREPARED slot's predecessor remains the active schema.
// Directory keys are persisted EntityKind ordinals, not namespace IDs.
// Metadata slot: generation:u64, reserved:u64, lastIssued:u64, clean:u8,
// 35 zero bytes, CRC32C:u32 over the first 60 bytes.
const (
	SchemaVersion     uint16 = 3
	previousVersion   uint16 = 2
	superMagic        uint32 = 0x49445343 // IDSC
	superBytes               = storage.SuperblockSlotBytes
	metadataBytes            = 64
	metadataPairBytes        = 2 * metadataBytes
	metaStart                = 2 * superBytes
	maxName                  = 255
	defaultReserve    uint64 = 1024
	maxDirectory             = 16 << 20
)

type directoryEntry struct {
	id      Namespace
	name    string
	reserve uint64
	offset  uint64
}

type allocationRecord struct {
	generation uint64
	reserved   uint64
	issued     uint64
	clean      bool
}

func encodeRecord(dst []byte, m allocationRecord) {
	clear(dst)
	binary.LittleEndian.PutUint64(dst[0:8], m.generation)
	binary.LittleEndian.PutUint64(dst[8:16], m.reserved)
	binary.LittleEndian.PutUint64(dst[16:24], m.issued)
	if m.clean {
		dst[24] = 1
	}
	binary.LittleEndian.PutUint32(dst[60:64], storage.Checksum(dst[:60]))
}

func decodeRecord(src []byte) (allocationRecord, bool) {
	if len(src) != metadataBytes || binary.LittleEndian.Uint32(src[60:64]) != storage.Checksum(src[:60]) {
		return allocationRecord{}, false
	}
	m := allocationRecord{binary.LittleEndian.Uint64(src[0:8]), binary.LittleEndian.Uint64(src[8:16]), binary.LittleEndian.Uint64(src[16:24]), src[24] == 1}
	if m.generation == 0 || src[24] > 1 || m.issued > m.reserved {
		return allocationRecord{}, false
	}
	for _, b := range src[25:60] {
		if b != 0 {
			return allocationRecord{}, false
		}
	}
	if !m.clean && m.issued != 0 {
		return allocationRecord{}, false
	}
	return m, true
}

func zeroRecord(src []byte) bool {
	for _, b := range src {
		if b != 0 {
			return false
		}
	}
	return true
}

func directoryBytes(entries []directoryEntry) ([]byte, error) {
	if len(entries) == 0 || len(entries) > math.MaxUint16 {
		return nil, ErrNamespace
	}
	size := 2 + 4
	for _, entry := range entries {
		size += 20 + len(entry.name)
	}
	if size > maxDirectory {
		return nil, ErrNamespace
	}
	data := make([]byte, size)
	binary.LittleEndian.PutUint16(data[:2], uint16(len(entries)))
	pos := 2
	for _, e := range entries {
		binary.LittleEndian.PutUint16(data[pos:pos+2], uint16(e.id))
		binary.LittleEndian.PutUint16(data[pos+2:pos+4], uint16(len(e.name)))
		binary.LittleEndian.PutUint64(data[pos+4:pos+12], e.reserve)
		binary.LittleEndian.PutUint64(data[pos+12:pos+20], e.offset)
		pos += 20
		copy(data[pos:], e.name)
		pos += len(e.name)
	}
	binary.LittleEndian.PutUint32(data[pos:], storage.Checksum(data[:pos]))
	return data, nil
}

func parseDirectory(data []byte, end uint64) ([]directoryEntry, error) {
	if len(data) < 26 || len(data) > maxDirectory || binary.LittleEndian.Uint32(data[len(data)-4:]) != storage.Checksum(data[:len(data)-4]) {
		return nil, ErrCorrupt
	}
	count := int(binary.LittleEndian.Uint16(data[:2]))
	if count == 0 {
		return nil, ErrCorrupt
	}
	entries := make([]directoryEntry, 0, count)
	pos := 2
	for i := 0; i < count; i++ {
		if pos+20 > len(data)-4 {
			return nil, ErrCorrupt
		}
		e := directoryEntry{Namespace(binary.LittleEndian.Uint16(data[pos : pos+2])), "", binary.LittleEndian.Uint64(data[pos+4 : pos+12]), binary.LittleEndian.Uint64(data[pos+12 : pos+20])}
		n := int(binary.LittleEndian.Uint16(data[pos+2 : pos+4]))
		pos += 20
		if n == 0 || n > maxName || pos+n > len(data)-4 || !e.id.Valid() || e.reserve == 0 ||
			e.offset < metaStart || e.offset%metadataBytes != 0 || e.offset > end || end-e.offset < metadataPairBytes ||
			(len(entries) > 0 && entries[len(entries)-1].id >= e.id) {
			return nil, ErrCorrupt
		}
		e.name = string(data[pos : pos+n])
		if !utf8.ValidString(e.name) {
			return nil, ErrCorrupt
		}
		pos += n
		for _, prior := range entries {
			if prior.name == e.name || (e.offset < prior.offset+metadataPairBytes && prior.offset < e.offset+metadataPairBytes) {
				return nil, ErrCorrupt
			}
		}
		entries = append(entries, e)
	}
	if pos != len(data)-4 {
		return nil, ErrCorrupt
	}
	return entries, nil
}

func superPayload(dirOffset uint64, data, definitions []byte, end uint64) [64]byte {
	var p [64]byte
	binary.LittleEndian.PutUint64(p[0:8], dirOffset)
	binary.LittleEndian.PutUint64(p[8:16], uint64(len(data)))
	binary.LittleEndian.PutUint32(p[16:20], storage.Checksum(data))
	binary.LittleEndian.PutUint16(p[20:22], binary.LittleEndian.Uint16(data[:2]))
	binary.LittleEndian.PutUint64(p[24:32], end)
	binary.LittleEndian.PutUint64(p[32:40], dirOffset+uint64(len(data)))
	binary.LittleEndian.PutUint64(p[40:48], uint64(len(definitions)))
	binary.LittleEndian.PutUint32(p[48:52], storage.Checksum(definitions))
	return p
}

func parseSuper(src []byte, size uint64) (uint64, uint64, uint64, uint64, error) {
	if len(src) != superBytes {
		return 0, 0, 0, 0, ErrCorrupt
	}
	version := binary.LittleEndian.Uint16(src[4:6])
	if binary.LittleEndian.Uint32(src[:4]) == superMagic && version != SchemaVersion && version != previousVersion {
		return 0, 0, 0, 0, ErrVersion
	}
	h, p, ok := storage.DecodeSuperblock(src, superMagic, version)
	if !ok || h.Generation == 0 || len(p) != 64 || p[22] != 0 || p[23] != 0 {
		return 0, 0, 0, 0, ErrCorrupt
	}
	off, length, end := binary.LittleEndian.Uint64(p[:8]), binary.LittleEndian.Uint64(p[8:16]), binary.LittleEndian.Uint64(p[24:32])
	if off < metaStart || length < 26 || length > maxDirectory || end > size || off > end || length > end-off || binary.LittleEndian.Uint16(p[20:22]) == 0 {
		return 0, 0, 0, 0, ErrCorrupt
	}
	defsOff, defsLen := binary.LittleEndian.Uint64(p[32:40]), binary.LittleEndian.Uint64(p[40:48])
	if defsOff != off+length || defsLen < 14 || defsLen > maxDirectory || defsOff > end || defsLen != end-defsOff {
		return 0, 0, 0, 0, ErrCorrupt
	}
	if version == previousVersion && (p[52] != 0 || p[53] != 0) || version == SchemaVersion && (p[52] > 1 || p[53] > 1) {
		return 0, 0, 0, 0, ErrCorrupt
	}
	for _, b := range p[54:] {
		if b != 0 {
			return 0, 0, 0, 0, ErrCorrupt
		}
	}
	return h.Generation, off, length, end, nil
}

func checkSuperDirectory(p, data, definitions []byte) error {
	if len(p) != 64 || binary.LittleEndian.Uint32(p[16:20]) != storage.Checksum(data) || binary.LittleEndian.Uint16(p[20:22]) != binary.LittleEndian.Uint16(data[:2]) || binary.LittleEndian.Uint32(p[48:52]) != storage.Checksum(definitions) {
		return ErrCorrupt
	}
	return nil
}

func formatErr(ns Namespace, err error) error {
	return fmt.Errorf("schema: namespace %d: %w", ns, err)
}

func unsupportedVersion(err error) bool { return errors.Is(err, ErrVersion) }
