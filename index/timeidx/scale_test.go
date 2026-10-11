package timeidx

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// Opt-in correctness gate for the same 1M/10M random-ID fixtures as benchmarks.
func TestScaleMutationCorrectness(t *testing.T) {
	if os.Getenv("TIMEIDX_SCALE") != "1" {
		t.Skip("set TIMEIDX_SCALE=1")
	}
	for _, count := range []int{1000000, 10000000} {
		for _, random := range []bool{false, true} {
			t.Run(fmt.Sprintf("N=%d/Random=%t", count, random), func(t *testing.T) {
				entries := scaleEntries(count, random, false)
				i, err := NewStore(t.TempDir(), Spec{Namespace: 1, Entries: entries}, Config{}, DurabilitySync)
				if err != nil {
					t.Fatal(err)
				}
				defer i.Close()
				expected := make([]uint64, count)
				for n, e := range entries {
					expected[n] = e.ImageID
				}
				slices.Sort(expected)
				for _, size := range []int{1, 8, 32, 64} {
					batch := NewBatch(size)
					for n := 0; n < size; n++ {
						pos := (count/2 + n*7919) % count
						ts := uint64(count+size*100+n) * 1000000000
						batch.Append(Move(entries[pos].ImageID, entries[pos].Timestamp, ts))
					}
					if err := i.Apply(batch); err != nil {
						t.Fatal(err)
					}
					if i.CommitStats().RewrittenShards > 2*size+2 {
						t.Fatal("unexpected rewrite", i.CommitStats())
					}
					for n := 0; n < size; n++ {
						pos := (count/2 + n*7919) % count
						ts := uint64(count+size*100+n) * 1000000000
						got := i.Search(nil, ts, ts+1)
						if len(got) != 1 || got[0] != entries[pos].ImageID {
							t.Fatal("wrong move", n, got)
						}
					}
					got := i.Search(nil, 0, ^uint64(0))
					slices.Sort(got)
					if !slices.Equal(got, expected) {
						t.Fatal("lost or duplicate IDs")
					}
				}
			})
		}
	}
}
