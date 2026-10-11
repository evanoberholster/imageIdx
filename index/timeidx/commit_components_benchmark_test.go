package timeidx

import (
	"fmt"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Phase measurements use the same 4 KiB/1M/10M fixtures as the full durable
// benchmark. No timers or diagnostic branches are added to production commits.
func BenchmarkCommitComponents(b *testing.B) {
	scaleEnabled(b)
	for _, count := range []int{1000000, 10000000} {
		b.Run(fmt.Sprintf("N=%d", count), func(b *testing.B) {
			i, err := NewStore(b.TempDir(), Spec{Namespace: 1, Entries: scaleEntries(count, false, false)}, Config{}, DurabilitySync)
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			batch := NewBatch(1)
			batch.Append(Move(scaleID(count/2, false), 0, uint64(count+1)*1000000000))
			if err := i.Apply(batch); err != nil {
				b.Fatal(err)
			}
			shards := i.view.Load().shards
			length := uint32(len(shards) * directoryEntrySize)
			ref, capacity, err := i.alloc.Alloc(length)
			if err != nil {
				b.Fatal(err)
			}
			defer i.alloc.Free(ref, capacity)
			dir := i.file.Bytes()[uint64(ref)*512 : uint64(ref)*512+uint64(length)]
			for _, zero := range []bool{true, false} {
				b.Run(fmt.Sprintf("Encode/Clear=%t", zero), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(length))
					b.ResetTimer()
					for k := 0; k < b.N; k++ {
						if zero {
							clear(dir)
						}
						for n, s := range shards {
							storage.EncodeShardDescriptor(dir[n*directoryEntrySize:(n+1)*directoryEntrySize], storage.ShardDescriptor{FirstKey: s.times[0], FirstID: s.ids[0], Offset: s.offset, Count: uint32(len(s.times)), KeyCRC: s.timeCRC, IDCRC: s.idCRC, MinID: s.minID, MaxID: s.maxID})
						}
					}
				})
			}
			b.Run("CRC", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(length))
				var crc uint32
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					crc = storage.Checksum(dir)
				}
				b.StopTimer()
				if crc != storage.Checksum(dir) {
					b.Fatal("CRC mismatch")
				}
			})
			b.Run("PreviousExtentList", func(b *testing.B) {
				b.ReportAllocs()
				var es []storage.Extent
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					es = shardLive(shards, i.root)
				}
				b.StopTimer()
				if len(es) != len(shards)+1 {
					b.Fatal("extent count")
				}
			})
			b.Run("Union", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					if _, err := storage.UnionExtents(i.live, i.live); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Reconstruct", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					if _, err := storage.ReconstructAllocator(i.OnDiskBytes(), i.live); err != nil {
						b.Fatal(err)
					}
				}
			})
			if err := i.alloc.Free(ref, capacity); err != nil {
				b.Fatal(err)
			}
			b.Run("AllocateFreeDirectory", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					r, c, err := i.alloc.Alloc(length)
					if err != nil {
						b.Fatal(err)
					}
					if err := i.alloc.Free(r, c); err != nil {
						b.Fatal(err)
					}
				}
			})
			if err := i.alloc.Reserve(ref, capacity); err != nil {
				b.Fatal(err)
			}
			b.Run("DirectorySync", func(b *testing.B) {
				// Dirty every page in an unpublished directory extent. This is
				// one real sync, not a claim to measure both commit barriers.
				b.ReportAllocs()
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					for off := 0; off < len(dir); off += 4096 {
						dir[off] ^= 1
					}
					if err := i.file.Sync(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
