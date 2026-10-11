package storage

import "encoding/binary"

const ShardDescriptorBytes = 56

// ShardDescriptor stores physical metadata only. Each index validates domain
// keys, checksums, bounds and ordering independently.
type ShardDescriptor struct {
	FirstKey, FirstID, Offset uint64
	Count, KeyCRC, IDCRC      uint32
	MinID, MaxID              uint64
}

func EncodeShardDescriptor(dst []byte, d ShardDescriptor) {
	_ = dst[55]
	binary.LittleEndian.PutUint64(dst[0:8], d.FirstKey)
	binary.LittleEndian.PutUint64(dst[8:16], d.FirstID)
	binary.LittleEndian.PutUint64(dst[16:24], d.Offset)
	binary.LittleEndian.PutUint32(dst[24:28], d.Count)
	binary.LittleEndian.PutUint32(dst[28:32], d.KeyCRC)
	binary.LittleEndian.PutUint32(dst[32:36], d.IDCRC)
	clear(dst[36:40])
	binary.LittleEndian.PutUint64(dst[40:48], d.MinID)
	binary.LittleEndian.PutUint64(dst[48:56], d.MaxID)
}

func DecodeShardDescriptor(src []byte) ShardDescriptor {
	_ = src[55]
	return ShardDescriptor{FirstKey: binary.LittleEndian.Uint64(src[0:8]), FirstID: binary.LittleEndian.Uint64(src[8:16]), Offset: binary.LittleEndian.Uint64(src[16:24]), Count: binary.LittleEndian.Uint32(src[24:28]), KeyCRC: binary.LittleEndian.Uint32(src[28:32]), IDCRC: binary.LittleEndian.Uint32(src[32:36]), MinID: binary.LittleEndian.Uint64(src[40:48]), MaxID: binary.LittleEndian.Uint64(src[48:56])}
}
