package hashidx

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// Fuzz targets cover the byte parsers that face untrusted store files.
// Each asserts totality (never panics) plus the structural invariants a
// successful decode must satisfy. Seed corpora run under plain go test;
// extended mutation runs happen on demand (e.g. go test -fuzz=...).

func validBlockImage(count uint32) []byte {
	payload := make([]byte, uint64(count)*RecordSize)
	for k := uint32(0); k < count; k++ {
		var h Hash
		binary.LittleEndian.PutUint64(h[24:32], uint64(k))
		encodeRecord(payload[k*RecordSize:(k+1)*RecordSize], ExactEntry{Hash: h, ImageID: common.ImageID(k + 1)})
	}
	image := make([]byte, blockHeaderSize+len(payload))
	encodeBlockHeader(image, count, crc32.ChecksumIEEE(payload))
	copy(image[blockHeaderSize:], payload)
	return image
}

func FuzzDecodeBlockHeader(f *testing.F) {
	f.Add(validBlockImage(0))
	f.Add(validBlockImage(2))
	f.Add([]byte{})
	f.Add([]byte("HBLK"))
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		count, err := decodeBlockHeader(data)
		if err != nil {
			return
		}
		// A successful decode guarantees the payload is present,
		// checksummed, and sorted.
		if uint64(count)*RecordSize > uint64(len(data))-blockHeaderSize {
			t.Fatalf("count %d overruns image of %d", count, len(data))
		}
		payload := data[blockHeaderSize : blockHeaderSize+uint64(count)*RecordSize]
		if !recordsSorted(payload) {
			t.Fatal("accepted unsorted records")
		}
	})
}

func validExactManifest() []byte {
	return encodeManifest(exactManifest{
		generation: 3, namespace: 7, liveCount: 11, blockCount: 2,
		walOffset: 128, flags: 0, blockSize: DefaultBlockSize,
		algorithm: AlgorithmSHA256, targetFillPct: DefaultTargetFillPct,
		walEpoch: 1,
	})
}

func FuzzDecodeManifest(f *testing.F) {
	f.Add(validExactManifest())
	f.Add([]byte{})
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := decodeManifest(data)
		if err != nil {
			return
		}
		// A successful decode guarantees the decoded discipline.
		if m.generation == 0 || m.walOffset < 0 || m.walEpoch == 0 || !m.algorithm.Valid() {
			t.Fatalf("accepted invalid manifest %+v", m)
		}
	})
}

func FuzzBlockLowerBound(f *testing.F) {
	f.Add(validBlockImage(3)[blockHeaderSize:], uint64(0))
	f.Add([]byte{}, uint64(0))
	f.Add([]byte{1, 2, 3}, uint64(99))
	f.Fuzz(func(t *testing.T, records []byte, seed uint64) {
		h := benchHash(seed)
		pos := blockLowerBound(records, h)
		n := len(records) / RecordSize
		if pos < 0 || pos > n {
			t.Fatalf("bound %d outside [0, %d]", pos, n)
		}
	})
}
