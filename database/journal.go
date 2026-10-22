package database

import (
	"encoding/binary"
	"hash/crc32"
	"math"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

// Journal payload v2: a typed, checksum-protected delta envelope.
//
//	version:u8 | flags:u8 | entity:u8 | valueID:u64
//	beforeCount:u16 | afterCount:u16 | beforeLen:u32 | afterLen:u32
//	beforeAssociations | afterAssociations | crc32:u32
//
// Each association is family:u8 | namespace:u16 | family payload. The before
// list is the set removed by this mutation; the after list is the set added.
// An insert is flagged explicitly so an empty final projection is not confused
// with a no-op update. Delete entries carry no association payload.
const (
	journalVersion = 2
	journalHeader  = 23

	journalFlagDeleted = 1 << 0
	journalFlagInsert  = 1 << 1
)

const (
	journalFamilyTime uint8 = iota + 1
	journalFamilyGeo
	journalFamilySim
	journalFamilyHash
	journalFamilyVector
	journalFamilyBitmap
)

type journalEntry struct {
	entity  schema.EntityKind
	id      schema.ValueID
	deleted bool
	insert  bool
	before  []byte
	after   []byte
	beforeN uint16
	afterN  uint16
}

// encodeMutation encodes typed before/after deltas. before is the set removed
// relative to the canonical prior state; after is the set added. insert means
// there was no prior canonical projection.
func encodeMutation(m Mutation, insert bool, before, after index.Entry) ([]byte, error) {
	beforeN := associationCount(before)
	afterN := associationCount(after)
	if beforeN > math.MaxUint16 || afterN > math.MaxUint16 {
		return nil, ErrInvalidMutation
	}
	if m.Delete {
		insert = false
		beforeN, afterN = 0, 0
	}
	capacity := journalHeader + 4
	if !m.Delete {
		capacity += associationSize(before) + associationSize(after)
	}
	b := make([]byte, journalHeader, capacity)
	if !m.Delete {
		var err error
		b, err = appendAssociations(b, before)
		if err != nil {
			return nil, err
		}
	}
	beforeLen := len(b) - journalHeader
	if !m.Delete {
		var err error
		b, err = appendAssociations(b, after)
		if err != nil {
			return nil, err
		}
	}
	afterLen := len(b) - journalHeader - beforeLen
	if len(b) > math.MaxUint32-4 {
		return nil, ErrInvalidMutation
	}
	b = append(b, 0, 0, 0, 0)
	b[0] = journalVersion
	if m.Delete {
		b[1] |= journalFlagDeleted
	}
	if insert {
		b[1] |= journalFlagInsert
	}
	b[2] = byte(m.Entity)
	binary.BigEndian.PutUint64(b[3:11], m.ValueID)
	binary.BigEndian.PutUint16(b[11:13], uint16(beforeN))
	binary.BigEndian.PutUint16(b[13:15], uint16(afterN))
	binary.BigEndian.PutUint32(b[15:19], uint32(beforeLen))
	binary.BigEndian.PutUint32(b[19:23], uint32(afterLen))
	binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
	return b, nil
}

func associationCount(entry index.Entry) int {
	return len(entry.Times) + len(entry.Geos) + len(entry.Sims) + len(entry.Hashes) + len(entry.Vectors) + len(entry.Bitmaps)
}

func associationSize(entry index.Entry) int {
	size := 0
	size += len(entry.Times) * 11
	size += len(entry.Geos) * 11
	size += len(entry.Sims) * (3 + 8 + 32 + 1)
	size += len(entry.Hashes) * (3 + 32)
	for _, value := range entry.Vectors {
		size += 3 + 1 + 4
		switch value.Element {
		case vectoridx.ElementInt8, vectoridx.ElementUint8:
			size += len(value.Int8) + len(value.Uint8)
		case vectoridx.ElementInt16, vectoridx.ElementUint16:
			size += 2 * (len(value.Int16) + len(value.Uint16))
		case vectoridx.ElementFloat32:
			size += 4 * len(value.Float32)
		}
	}
	for _, value := range entry.Bitmaps {
		size += 3 + 1 + 8 + 1 + 4 + len(value.Value.Bytes)
	}
	return size
}

func decodeMutation(b []byte) (journalEntry, error) {
	if len(b) < journalHeader+4 {
		return journalEntry{}, ErrCorruptJournal
	}
	if b[0] != journalVersion {
		if b[0] == 1 {
			return journalEntry{}, ErrUnsupportedJournalVersion
		}
		return journalEntry{}, ErrCorruptJournal
	}
	flags := b[1]
	if flags&^(journalFlagDeleted|journalFlagInsert) != 0 {
		return journalEntry{}, ErrCorruptJournal
	}
	entity := schema.EntityKind(b[2])
	id := binary.BigEndian.Uint64(b[3:11])
	beforeN := binary.BigEndian.Uint16(b[11:13])
	afterN := binary.BigEndian.Uint16(b[13:15])
	beforeLen := uint64(binary.BigEndian.Uint32(b[15:19]))
	afterLen := uint64(binary.BigEndian.Uint32(b[19:23]))
	if !entity.Valid() || id == 0 ||
		beforeLen+afterLen != uint64(len(b)-journalHeader-4) ||
		binary.BigEndian.Uint32(b[len(b)-4:]) != crc32.ChecksumIEEE(b[:len(b)-4]) {
		return journalEntry{}, ErrCorruptJournal
	}
	deleted := flags&journalFlagDeleted != 0
	insert := flags&journalFlagInsert != 0
	if deleted && (afterLen != 0 || afterN != 0) {
		return journalEntry{}, ErrCorruptJournal
	}
	if insert && (beforeLen != 0 || beforeN != 0 || deleted) {
		return journalEntry{}, ErrCorruptJournal
	}
	beforeStart := journalHeader
	afterStart := beforeStart + int(beforeLen)
	return journalEntry{
		entity:  entity,
		id:      id,
		deleted: deleted,
		insert:  insert,
		before:  b[beforeStart:afterStart],
		after:   b[afterStart : len(b)-4],
		beforeN: beforeN,
		afterN:  afterN,
	}, nil
}

func appendAssociations(dst []byte, entry index.Entry) ([]byte, error) {
	if associationCount(entry) > math.MaxUint16 {
		return nil, ErrInvalidMutation
	}
	for _, value := range entry.Times {
		dst = appendFamily(dst, journalFamilyTime, value.Namespace)
		dst = binary.BigEndian.AppendUint64(dst, value.Timestamp)
	}
	for _, value := range entry.Geos {
		dst = appendFamily(dst, journalFamilyGeo, value.Namespace)
		dst = binary.BigEndian.AppendUint64(dst, uint64(value.Key))
	}
	for _, value := range entry.Sims {
		dst = appendFamily(dst, journalFamilySim, value.Namespace)
		dst = binary.BigEndian.AppendUint64(dst, value.Record.PHash)
		dst = append(dst, value.Record.PDQ[:]...)
		dst = append(dst, value.Record.Quality)
	}
	for _, value := range entry.Hashes {
		dst = appendFamily(dst, journalFamilyHash, value.Namespace)
		dst = append(dst, value.Hash[:]...)
	}
	for _, value := range entry.Vectors {
		var err error
		dst, err = appendVector(dst, value)
		if err != nil {
			return nil, err
		}
	}
	for _, value := range entry.Bitmaps {
		dst = appendFamily(dst, journalFamilyBitmap, value.Namespace)
		dst = append(dst, byte(value.Value.Kind))
		dst = binary.BigEndian.AppendUint64(dst, uint64(value.Value.Int))
		if value.Value.Bool {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
		if len(value.Value.Bytes) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Value.Bytes)))
		dst = append(dst, value.Value.Bytes...)
	}
	return dst, nil
}

func appendFamily(dst []byte, family uint8, ns schema.Namespace) []byte {
	dst = append(dst, family)
	return binary.BigEndian.AppendUint16(dst, uint16(ns))
}

func appendVector(dst []byte, value index.VectorValue) ([]byte, error) {
	dst = appendFamily(dst, journalFamilyVector, value.Namespace)
	dst = append(dst, byte(value.Element))
	switch value.Element {
	case vectoridx.ElementInt8:
		if len(value.Int8) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Int8)))
		for _, v := range value.Int8 {
			dst = append(dst, byte(v))
		}
	case vectoridx.ElementUint8:
		if len(value.Uint8) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Uint8)))
		dst = append(dst, value.Uint8...)
	case vectoridx.ElementInt16:
		if len(value.Int16) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Int16)))
		for _, v := range value.Int16 {
			dst = binary.BigEndian.AppendUint16(dst, uint16(v))
		}
	case vectoridx.ElementUint16:
		if len(value.Uint16) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Uint16)))
		for _, v := range value.Uint16 {
			dst = binary.BigEndian.AppendUint16(dst, v)
		}
	case vectoridx.ElementFloat32:
		if len(value.Float32) > math.MaxUint32 {
			return nil, ErrInvalidMutation
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(value.Float32)))
		for _, v := range value.Float32 {
			dst = binary.BigEndian.AppendUint32(dst, math.Float32bits(v))
		}
	default:
		return nil, ErrInvalidMutation
	}
	return dst, nil
}

func decodeAssociations(data []byte, count uint16, dst *index.Entry) error {
	pos := 0
	for i := uint16(0); i < count; i++ {
		if pos+3 > len(data) {
			return ErrCorruptJournal
		}
		family := data[pos]
		ns := schema.Namespace(binary.BigEndian.Uint16(data[pos+1 : pos+3]))
		pos += 3
		switch family {
		case journalFamilyTime:
			if pos+8 > len(data) {
				return ErrCorruptJournal
			}
			dst.Times = append(dst.Times, index.TimeValue{Namespace: ns, Timestamp: binary.BigEndian.Uint64(data[pos : pos+8])})
			pos += 8
		case journalFamilyGeo:
			if pos+8 > len(data) {
				return ErrCorruptJournal
			}
			dst.Geos = append(dst.Geos, index.GeoValue{Namespace: ns, Key: geoidx.S2Key(binary.BigEndian.Uint64(data[pos : pos+8]))})
			pos += 8
		case journalFamilySim:
			if pos+8+32+1 > len(data) {
				return ErrCorruptJournal
			}
			var pdq [32]byte
			copy(pdq[:], data[pos+8:pos+8+32])
			dst.Sims = append(dst.Sims, index.SimValue{
				Namespace: ns,
				Record: simhash.Record{
					PHash:   binary.BigEndian.Uint64(data[pos : pos+8]),
					PDQ:     pdq,
					Quality: data[pos+8+32],
				},
			})
			pos += 8 + 32 + 1
		case journalFamilyHash:
			if pos+32 > len(data) {
				return ErrCorruptJournal
			}
			var hash radixhash.Hash
			copy(hash[:], data[pos:pos+32])
			dst.Hashes = append(dst.Hashes, index.HashValue{Namespace: ns, Hash: hash})
			pos += 32
		case journalFamilyVector:
			next, err := decodeVector(data, pos, ns, dst)
			if err != nil {
				return err
			}
			pos = next
		case journalFamilyBitmap:
			if pos+1+8+1+4 > len(data) {
				return ErrCorruptJournal
			}
			kind := schema.NamespaceKind(data[pos])
			value := int64(binary.BigEndian.Uint64(data[pos+1 : pos+9]))
			isBool := data[pos+9] != 0
			n := binary.BigEndian.Uint32(data[pos+10 : pos+14])
			pos += 14
			if uint64(n) > uint64(len(data)-pos) {
				return ErrCorruptJournal
			}
			var bytesValue []byte
			if n != 0 {
				bytesValue = data[pos : pos+int(n)]
			}
			dst.Bitmaps = append(dst.Bitmaps, index.BitmapValue{
				Namespace: ns,
				Value: bitmapidx.Value{
					Kind:  kind,
					Bytes: bytesValue,
					Int:   value,
					Bool:  isBool,
				},
			})
			pos += int(n)
		default:
			return ErrCorruptJournal
		}
	}
	if pos != len(data) {
		return ErrCorruptJournal
	}
	return nil
}

func decodeVector(data []byte, pos int, ns schema.Namespace, dst *index.Entry) (int, error) {
	if pos+5 > len(data) {
		return 0, ErrCorruptJournal
	}
	element := vectoridx.ElementType(data[pos])
	n := binary.BigEndian.Uint32(data[pos+1 : pos+5])
	pos += 5
	vector := index.VectorValue{Namespace: ns, Element: element}
	switch element {
	case vectoridx.ElementInt8:
		if uint64(n) > uint64(len(data)-pos) {
			return 0, ErrCorruptJournal
		}
		vector.Int8 = make([]int8, n)
		for i := range vector.Int8 {
			vector.Int8[i] = int8(data[pos+i])
		}
		pos += int(n)
	case vectoridx.ElementUint8:
		if uint64(n) > uint64(len(data)-pos) {
			return 0, ErrCorruptJournal
		}
		vector.Uint8 = data[pos : pos+int(n)]
		pos += int(n)
	case vectoridx.ElementInt16:
		if uint64(n)*2 > uint64(len(data)-pos) {
			return 0, ErrCorruptJournal
		}
		vector.Int16 = make([]int16, n)
		for i := range vector.Int16 {
			vector.Int16[i] = int16(binary.BigEndian.Uint16(data[pos+2*i : pos+2*i+2]))
		}
		pos += int(n) * 2
	case vectoridx.ElementUint16:
		if uint64(n)*2 > uint64(len(data)-pos) {
			return 0, ErrCorruptJournal
		}
		vector.Uint16 = make([]uint16, n)
		for i := range vector.Uint16 {
			vector.Uint16[i] = binary.BigEndian.Uint16(data[pos+2*i : pos+2*i+2])
		}
		pos += int(n) * 2
	case vectoridx.ElementFloat32:
		if uint64(n)*4 > uint64(len(data)-pos) {
			return 0, ErrCorruptJournal
		}
		vector.Float32 = make([]float32, n)
		for i := range vector.Float32 {
			vector.Float32[i] = math.Float32frombits(binary.BigEndian.Uint32(data[pos+4*i : pos+4*i+4]))
		}
		pos += int(n) * 4
	default:
		return 0, ErrCorruptJournal
	}
	dst.Vectors = append(dst.Vectors, vector)
	return pos, nil
}
