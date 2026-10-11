package timeidx

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
)

// Opt-in: setup includes 10M rows and real mmap writes. This file uses APIs and
// private prepare signatures present at reviewed HEAD so it runs unchanged there.
func scaleEnabled(b *testing.B) {
	b.Helper()
	if os.Getenv("TIMEIDX_SCALE") != "1" {
		b.Skip("set TIMEIDX_SCALE=1")
	}
}

// An invertible uint64 permutation supplies unique, full-width random-looking
// IDs without a map, a shuffle allocation, or accidental duplicates.
func scaleID(n int, random bool) uint64 {
	v := uint64(n + 1)
	if random {
		v ^= v >> 30
		v *= 0xbf58476d1ce4e5b9
		v ^= v >> 27
		v *= 0x94d049bb133111eb
		v ^= v >> 31
	}
	return v
}

func scaleEntries(count int, random, repeated bool) []Entry {
	entries := make([]Entry, count)
	for n := range entries {
		ts := uint64(n) * 1000000000
		if repeated {
			ts = uint64(n/10000) * 1000000000
		}
		entries[n] = Entry{Timestamp: ts, ImageID: scaleID(n, random)}
	}
	return entries
}

// Detection instrumentation is a separate untimed pass over the same fixed
// fixture. It counts records inspected while finding old IDs, not comparison
// callbacks in merging. The production implementation has no metric branches.
func detectionWork(shards []*shard, ops []Mutation) (scanned, inspected int) {
	for _, s := range shards {
		if s.maxID < ops[0].imageID || s.minID > ops[len(ops)-1].imageID {
			continue
		}
		scanned++
		for _, id := range s.ids {
			inspected++
			if _, ok := slices.BinarySearchFunc(ops, id, func(a Mutation, b uint64) int { return cmp.Compare(a.imageID, b) }); ok {
				break
			}
		}
	}
	return
}

func BenchmarkScalePreparation(b *testing.B) {
	scaleEnabled(b)
	for _, count := range []int{1000000, 10000000} {
		for _, random := range []bool{false, true} {
			b.Run(fmt.Sprintf("N=%d/Random=%t", count, random), func(b *testing.B) {
				shards, err := initialShards(scaleEntries(count, random, false), 256)
				if err != nil {
					b.Fatal(err)
				}
				for n, s := range shards {
					s.offset = uint64(n+2) * 4096
				}
				for _, batchSize := range []int{1, 8, 32, 64} {
					b.Run(fmt.Sprintf("Batch=%d", batchSize), func(b *testing.B) {
						ops := make([]Mutation, batchSize)
						for n := range ops {
							pos := (count/2 + n*7919) % count
							ops[n] = Delete(scaleID(pos, random), 0)
						}
						slices.SortFunc(ops, func(a, b Mutation) int { return cmp.Compare(a.imageID, b.imageID) })
						scanned, inspected := detectionWork(shards, ops)
						var scratch writeScratch
						plans := prepare(shards, ops, 256, &scratch, true)
						rewritten := 0
						rows := 0
						for _, s := range plans {
							rows += len(s.times)
							if s.offset == 0 {
								rewritten++
							}
						}
						if rows != count-batchSize || rewritten > batchSize {
							b.Fatal("incorrect or whole-index deletion", rows, rewritten)
						}
						b.ReportAllocs()
						b.ResetTimer()
						for n := 0; n < b.N; n++ {
							plans = prepare(shards, ops, 256, &scratch, true)
						}
						b.StopTimer()
						b.ReportMetric(float64(len(shards)), "shards-visited")
						b.ReportMetric(float64(scanned), "shards-scanned")
						b.ReportMetric(float64(inspected), "detection-IDs")
						b.ReportMetric(float64(rewritten), "shards-rewritten")
					})
				}
			})
		}
	}
}

func BenchmarkScaleShards(b *testing.B) {
	scaleEnabled(b)
	for _, count := range []int{1000000, 10000000} {
		for _, random := range []bool{false, true} {
			for _, repeated := range []bool{false, true} {
				entries := scaleEntries(count, random, repeated)
				for _, size := range []int64{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20} {
					b.Run(fmt.Sprintf("N=%d/Random=%t/Repeated=%t/Bytes=%d", count, random, repeated, size), func(b *testing.B) {
						// Sharing one setup between subbenchmarks bounds setup cost; each
						// case closes mappings before moving to the next configuration.
						dir := b.TempDir()
						remaps := 0
						i, err := createStore(dir, Spec{Namespace: 1, Entries: entries}, Config{MaxShardBytes: size, MaxFileBytes: 4 << 20}, DurabilitySync, func(i *Index) {
							i.crash = func(stage string) {
								if stage == "grow" {
									remaps++
								}
							}
						})
						if err != nil {
							b.Fatal(err)
						}
						defer func() { i.Close() }()
						start := entries[count/2].Timestamp
						dst := make([]common.ValueID, 0, count)
						for _, width := range []uint64{1, (entries[count-1].Timestamp + 1000000000) / 4} {
							b.Run(fmt.Sprintf("Search/Width=%d", width), func(b *testing.B) {
								b.ReportAllocs()
								b.ResetTimer()
								for n := 0; n < b.N; n++ {
									dst = i.Search(dst[:0], start, start+width)
								}
								b.StopTimer()
								b.ReportMetric(float64(len(dst)), "matches/op")
							})
						}
						for _, historical := range []bool{false, true} {
							sequence := uint64(0) // survives benchmark calibration calls
							b.Run(fmt.Sprintf("Commit64/Historical=%t", historical), func(b *testing.B) {
								batch := NewBatch(64)
								var logical uint64
								b.ReportAllocs()
								b.ResetTimer()
								for k := 0; k < b.N; k++ {
									sequence++
									batch.Reset()
									for n := 0; n < 64; n++ {
										pos := (count/2 + n*7919) % count
										ts := (uint64(count) + sequence*64 + uint64(n)) * 1000000000
										if historical {
											ts = entries[(pos+int(sequence))%count].Timestamp + sequence
										}
										batch.Append(Insert(scaleID(pos, random), ts))
									}
									if err := i.Apply(batch); err != nil {
										b.Fatal(err)
									}
									logical += i.CommitStats().LogicalBytesWritten
									if i.Len() != uint64(count) {
										b.Fatal("upsert changed cardinality")
									}
								}
								b.StopTimer()
								b.ReportMetric(float64(logical)/float64(b.N), "logical-B/op")
							})
						}
						fileBytes := i.OnDiskBytes()
						b.Run("Reopen", func(b *testing.B) {
							i.Close()
							b.ReportAllocs()
							b.ResetTimer()
							for n := 0; n < b.N; n++ {
								i, err = OpenNamespace(dir, 1, Config{MaxShardBytes: size}, DurabilitySync)
								if err != nil {
									b.Fatal(err)
								}
								i.Close()
							}
						})
						b.Run("Storage", func(b *testing.B) {
							b.ReportMetric(float64(fileBytes), "file-B")
							b.ReportMetric(float64(len(i.view.Load().shards)), "shards")
							b.ReportMetric(float64(len(i.view.Load().shards)*directoryEntrySize), "directory-B")
							b.ReportMetric(float64(i.alloc.FreeBytes()), "free-B")
							b.ReportMetric(float64(i.alloc.LargestFreeBytes()), "largest-free-B")
							b.ReportMetric(float64(remaps), "remaps")
						})
					})
				}
			}
		}
	}
}

func BenchmarkScaleDurableBatches(b *testing.B) {
	scaleEnabled(b)
	for _, count := range []int{1000000, 10000000} {
		for _, random := range []bool{false, true} {
			b.Run(fmt.Sprintf("N=%d/Random=%t", count, random), func(b *testing.B) {
				entries := scaleEntries(count, random, false)
				i, err := NewStore(b.TempDir(), Spec{Namespace: 1, Entries: entries}, Config{}, DurabilitySync)
				if err != nil {
					b.Fatal(err)
				}
				defer i.Close()
				sequence := uint64(0)
				for _, size := range []int{1, 8, 32, 64} {
					b.Run(fmt.Sprintf("Batch=%d", size), func(b *testing.B) {
						batch := NewBatch(size)
						var logical uint64
						b.ReportAllocs()
						b.ResetTimer()
						for k := 0; k < b.N; k++ {
							sequence++
							batch.Reset()
							for n := 0; n < size; n++ {
								pos := (count/2 + n*7919) % count
								batch.Append(Insert(scaleID(pos, random), entries[pos].Timestamp+sequence))
							}
							if err := i.Apply(batch); err != nil {
								b.Fatal(err)
							}
							logical += i.CommitStats().LogicalBytesWritten
						}
						b.StopTimer()
						b.ReportMetric(float64(logical)/float64(b.N), "logical-B/op")
					})
				}
			})
		}
	}
}

// Unlike tail moves, this workload grows cardinality with genuinely new IDs.
func BenchmarkScaleChronologicalInsert(b *testing.B) {
	scaleEnabled(b)
	for _, count := range []int{1000000, 10000000} {
		for _, random := range []bool{false, true} {
			entries := scaleEntries(count, random, false)
			for _, size := range []int64{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20} {
				b.Run(fmt.Sprintf("N=%d/Random=%t/Bytes=%d", count, random, size), func(b *testing.B) {
					remaps := 0
					i, err := createStore(b.TempDir(), Spec{Namespace: 1, Entries: entries}, Config{MaxShardBytes: size, MaxFileBytes: 4 << 20}, DurabilitySync, func(i *Index) {
						i.crash = func(stage string) {
							if stage == "grow" {
								remaps++
							}
						}
					})
					if err != nil {
						b.Fatal(err)
					}
					defer i.Close()
					initialRemaps := remaps
					batch := NewBatch(64)
					var logical uint64
					b.ReportAllocs()
					b.ResetTimer()
					for k := 0; k < b.N; k++ {
						batch.Reset()
						for n := 0; n < 64; n++ {
							pos := count + k*64 + n
							batch.Append(Insert(scaleID(pos, random), uint64(pos)*1000000000))
						}
						if err := i.Apply(batch); err != nil {
							b.Fatal(err)
						}
						logical += i.CommitStats().LogicalBytesWritten
					}
					b.StopTimer()
					if i.Len() != uint64(count+b.N*64) {
						b.Fatal("incorrect insert cardinality")
					}
					b.ReportMetric(float64(logical)/float64(b.N), "logical-B/op")
					b.ReportMetric(float64(remaps-initialRemaps), "commit-remaps")
					b.ReportMetric(float64(i.OnDiskBytes()), "file-B")
				})
			}
		}
	}
}
