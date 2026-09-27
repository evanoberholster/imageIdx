package store

import (
	"encoding/binary"
	"errors"

	"github.com/evanoberholster/imageIdx/common"
)

// Key is the complete identity of one value. The on-disk encoding is always
// 12 bytes: big-endian namespace, ID, then flags.
type Key struct {
	Namespace common.Namespace
	ID        uint64
	Flags     uint16
}

const KeySize = 12

var ErrInvalidKey = errors.New("store: invalid key length")

// EncodeKey returns the fixed-width, lexicographically ordered key.
func EncodeKey(key Key) [KeySize]byte {
	var encoded [KeySize]byte
	binary.BigEndian.PutUint16(encoded[0:2], uint16(key.Namespace))
	binary.BigEndian.PutUint64(encoded[2:10], key.ID)
	binary.BigEndian.PutUint16(encoded[10:12], key.Flags)
	return encoded
}

// DecodeKey decodes a key produced by EncodeKey.
func DecodeKey(encoded []byte) (Key, error) {
	if len(encoded) != KeySize {
		return Key{}, ErrInvalidKey
	}
	return Key{
		Namespace: common.Namespace(binary.BigEndian.Uint16(encoded[0:2])),
		ID:        binary.BigEndian.Uint64(encoded[2:10]),
		Flags:     binary.BigEndian.Uint16(encoded[10:12]),
	}, nil
}
