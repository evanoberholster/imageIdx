package roar

import "bytes"

// MarshalBinary returns an owned copy of the serialized representation. The
// returned bytes are independent of the Bitmap's backing storage, so callers
// may persist or hand them off while the original mmap is retained or freed.
// It implements encoding.BinaryMarshaler.
func (b Bitmap) MarshalBinary() ([]byte, error) {
	if len(b.data) == 0 {
		return assemble(nil, nil, nil), nil
	}
	cloned := bytes.Clone(b.data)
	requireAligned(cloned)
	return cloned, nil
}

// UnmarshalBinary validates data and replaces the receiver with a Bitmap over
// it. It implements encoding.BinaryUnmarshaler.
func (b *Bitmap) UnmarshalBinary(data []byte) error {
	parsed, err := Open(data)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// Clone returns a Bitmap over an owned copy of the backing bytes. It is
// useful for decoupling a Bitmap from an mmap or a shared buffer.
func (b Bitmap) Clone() Bitmap {
	if len(b.data) == 0 {
		return Bitmap{}
	}
	cloned := bytes.Clone(b.data)
	requireAligned(cloned)
	return Bitmap{data: cloned}
}

// Bytes returns the underlying serialized bytes without copying. The caller
// must not modify the returned slice or free the backing storage while the
// Bitmap is in use.
func (b Bitmap) Bytes() []byte {
	return b.data
}

// LenInBytes returns the size of the serialized representation in bytes.
func (b Bitmap) LenInBytes() int {
	return len(b.data)
}
