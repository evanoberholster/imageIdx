package storage

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	// SuperblockSlots is the number of alternating superblock slots an index
	// keeps so a crash mid-checkpoint leaves the previous generation valid.
	SuperblockSlots = 2
	// SuperblockSlotBytes is the fixed size of one superblock slot.
	SuperblockSlotBytes = 512
	// superblockHeaderBytes is magic(4) + version(2) + flags(2) +
	// generation(8) + payload length(4) + payload CRC(4) + header CRC(4).
	superblockHeaderBytes = 28
)

// ErrSuperblockSize reports a slot or payload that does not fit the fixed
// superblock envelope.
var ErrSuperblockSize = errors.New("storage: superblock size")

// SuperblockHeader is the common envelope around an index-specific checkpoint
// payload. It carries identity, format version, and generation; the payload is
// opaque and owned by the index package (for example a root reference and
// record counts).
type SuperblockHeader struct {
	Magic      uint32
	Version    uint16
	Flags      uint16
	Generation uint64
	PayloadLen uint32
	PayloadCRC uint32
}

// EncodeSuperblock writes header and payload into one fixed-size slot. The
// payload gets its own CRC32 and the header gets a CRC32 over its own fields,
// so a torn or corrupt slot is rejected on decode. dst must be exactly
// SuperblockSlotBytes and the payload must fit beside the header.
func EncodeSuperblock(dst []byte, header SuperblockHeader, payload []byte) error {
	if len(dst) != SuperblockSlotBytes || superblockHeaderBytes+len(payload) > SuperblockSlotBytes {
		return ErrSuperblockSize
	}
	binary.LittleEndian.PutUint32(dst[0:4], header.Magic)
	binary.LittleEndian.PutUint16(dst[4:6], header.Version)
	binary.LittleEndian.PutUint16(dst[6:8], header.Flags)
	binary.LittleEndian.PutUint64(dst[8:16], header.Generation)
	binary.LittleEndian.PutUint32(dst[16:20], uint32(len(payload)))
	binary.LittleEndian.PutUint32(dst[20:24], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(dst[24:28], crc32.ChecksumIEEE(dst[:24]))
	copy(dst[superblockHeaderBytes:], payload)
	clear(dst[superblockHeaderBytes+len(payload):])
	return nil
}

// DecodeSuperblock validates a slot and returns its header and payload. ok is
// false for an empty, mismatched, or corrupt slot; a recognized magic with a
// different version is treated as not-this-format rather than corrupt.
func DecodeSuperblock(src []byte, magic uint32, version uint16) (SuperblockHeader, []byte, bool) {
	if len(src) < superblockHeaderBytes {
		return SuperblockHeader{}, nil, false
	}
	header := SuperblockHeader{
		Magic:      binary.LittleEndian.Uint32(src[0:4]),
		Version:    binary.LittleEndian.Uint16(src[4:6]),
		Flags:      binary.LittleEndian.Uint16(src[6:8]),
		Generation: binary.LittleEndian.Uint64(src[8:16]),
		PayloadLen: binary.LittleEndian.Uint32(src[16:20]),
		PayloadCRC: binary.LittleEndian.Uint32(src[20:24]),
	}
	if header.Magic != magic || header.Version != version {
		return SuperblockHeader{}, nil, false
	}
	if header.PayloadLen > SuperblockSlotBytes-superblockHeaderBytes {
		return SuperblockHeader{}, nil, false
	}
	if binary.LittleEndian.Uint32(src[24:28]) != crc32.ChecksumIEEE(src[:24]) {
		return SuperblockHeader{}, nil, false
	}
	payload := src[superblockHeaderBytes : superblockHeaderBytes+int(header.PayloadLen)]
	if header.PayloadCRC != crc32.ChecksumIEEE(payload) {
		return SuperblockHeader{}, nil, false
	}
	return header, payload, true
}

// SelectSuperblock decodes two slots and returns the one with the highest valid
// generation. It lets an opener choose the newest committed checkpoint without
// knowing the payload layout; the caller still validates the payload contents.
func SelectSuperblock(slots [SuperblockSlots][]byte, magic uint32, version uint16) (int, SuperblockHeader, []byte, bool) {
	best := -1
	var (
		bestHeader  SuperblockHeader
		bestPayload []byte
	)
	for slot, data := range slots {
		header, payload, ok := DecodeSuperblock(data, magic, version)
		if !ok {
			continue
		}
		if best == -1 || header.Generation > bestHeader.Generation {
			best, bestHeader, bestPayload = slot, header, payload
		}
	}
	if best == -1 {
		return 0, SuperblockHeader{}, nil, false
	}
	return best, bestHeader, bestPayload, true
}
