package timeidx

import (
	"encoding/binary"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func FuzzNamespaceGeneration(f *testing.F) {
	f.Add([]byte{}, []byte{}, uint64(1))
	f.Add(make([]byte, 4096), make([]byte, payloadBytes), uint64(1))
	p := make([]byte, payloadBytes)
	binary.LittleEndian.PutUint16(p[:2], 1)
	binary.LittleEndian.PutUint16(p[2:4], 1)
	binary.LittleEndian.PutUint32(p[20:24], storage.Checksum(nil))
	binary.LittleEndian.PutUint64(p[32:40], 4096)
	binary.LittleEndian.PutUint64(p[48:56], 64<<10)
	f.Add(make([]byte, 4096), p, uint64(1))
	f.Fuzz(func(t *testing.T, data, payload []byte, gen uint64) {
		g, err := decodeGeneration(data, payload, 1, gen)
		if err == nil {
			if g.view == nil {
				t.Fatal("nil view")
			}
			if _, err := storage.ReconstructAllocator(uint64(len(data)), g.live); err != nil {
				t.Fatal(err)
			}
		}
	})
}
