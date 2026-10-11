package storage

import (
	"encoding/binary"
	"testing"
)

func TestShardDescriptorBinaryLayout(t *testing.T) {
	d := ShardDescriptor{FirstKey: 1, FirstID: 2, Offset: 1024, Count: 3, KeyCRC: 4, IDCRC: 5, MinID: 6, MaxID: 7}
	var data [ShardDescriptorBytes]byte
	for n := range data {
		data[n] = 255
	}
	EncodeShardDescriptor(data[:], d)
	if DecodeShardDescriptor(data[:]) != d {
		t.Fatal("roundtrip")
	}
	if binary.LittleEndian.Uint32(data[36:40]) != 0 || binary.LittleEndian.Uint64(data[16:24]) != 1024 {
		t.Fatal("layout")
	}
}
