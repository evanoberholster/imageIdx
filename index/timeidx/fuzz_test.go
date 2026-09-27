package timeidx

import (
	"encoding/binary"
	"testing"
)

// Fuzz targets cover the byte parsers that face untrusted store files.
// Each asserts totality (never panics) plus the structural invariants a
// successful decode must satisfy. Seed corpora run under plain go test;
// extended mutation runs happen on demand (e.g. go test -fuzz=...).

func validManifestBytes() []byte {
	return encodeManifest(diskManifest{
		generation: 3, namespace: 7, count: 11,
		startTime: 1000, endTime: 2000, offsetTime: 1000,
		walOffset: 128, blockCount: 2, flags: 0, walEpoch: 1,
	})
}

func FuzzDecodeManifest(f *testing.F) {
	f.Add(validManifestBytes())
	f.Add([]byte{})
	f.Add(make([]byte, 88))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := decodeManifest(data)
		if err != nil {
			return
		}
		// A successful decode guarantees the decoded discipline.
		if m.generation == 0 || m.walOffset < 0 || m.walEpoch == 0 {
			t.Fatalf("accepted invalid manifest %+v", m)
		}
	})
}

func validWALHeader(count int) []byte {
	header := make([]byte, walHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], walMagic)
	binary.LittleEndian.PutUint16(header[4:6], walVersion)
	binary.LittleEndian.PutUint16(header[6:8], uint16(count))
	binary.LittleEndian.PutUint32(header[8:12], uint32(count*walWriteSize))
	// CRC over empty payload; validateWALFrame never reads past [8:12].
	return header
}

func FuzzValidateWALFrame(f *testing.F) {
	f.Add(validWALHeader(0))
	f.Add(validWALHeader(3))
	f.Add([]byte{})
	f.Add([]byte("TSWL"))
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, header []byte) {
		size, err := validateWALFrame(header)
		if err != nil {
			return
		}
		count := int(binary.LittleEndian.Uint16(header[6:8]))
		if size != uint64(count*walWriteSize) {
			t.Fatalf("size %d disagrees with count %d", size, count)
		}
	})
}

func FuzzMarkerRange(f *testing.F) {
	f.Add(uint64(1000), uint64(2000), uint64(1000))
	f.Add(uint64(0), uint64(0), uint64(0))
	f.Add(^uint64(0), ^uint64(0), uint64(0))
	f.Fuzz(func(t *testing.T, start, end, offset uint64) {
		lo, hi, ok := markerRange(start, end, offset)
		if !ok {
			return
		}
		// A successful range is non-empty and ordered.
		if !(lo < hi) {
			t.Fatalf("empty range [%d, %d) from (%d, %d, %d)", lo, hi, start, end, offset)
		}
	})
}
