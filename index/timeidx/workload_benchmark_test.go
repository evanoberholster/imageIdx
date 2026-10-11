package timeidx

import (
	"fmt"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
)

// This file deliberately uses only the pre-modernization public API so the
// identical harness can run at the baseline revision as well as the new one.
func BenchmarkModernization(b *testing.B) {
	const n = 100000
	const base = uint64(1_700_000_000_000_000_000)
	const hour = uint64(3600 * 1_000_000_000)
	const spacing = (365 * 24 * hour / n) >> TimestampShift << TimestampShift
	entries := make([]Entry, n)
	for k := range entries {
		entries[k] = Entry{Timestamp: base + uint64(k)*spacing, ImageID: uint64(k + 1)}
	}
	for _, tc := range []struct {
		name  string
		width uint64
	}{
		{"Exact", 1}, {"Hour", hour}, {"Day", 24 * hour}, {"Month", 30 * 24 * hour},
	} {
		b.Run("Search/"+tc.name, func(b *testing.B) {
			i, err := NewMemory(Spec{Offset: base, Entries: entries}, Config{})
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			start := base + 50000*spacing
			dst := make([]common.ValueID, 0, n)
			expected := len(i.Search(dst[:0], start, start+tc.width))
			b.ReportAllocs()
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				dst = i.Search(dst[:0], start, start+tc.width)
				if len(dst) != expected {
					b.Fatal("result changed")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(expected), "matches/op")
		})
	}
	for _, duplicates := range []bool{false, true} {
		b.Run(fmt.Sprintf("Lookup/Duplicates=%t", duplicates), func(b *testing.B) {
			data := entries
			if duplicates {
				data = make([]Entry, n)
				for k := range data {
					data[k] = Entry{Timestamp: base, ImageID: uint64(k + 1)}
				}
			}
			i, err := NewMemory(Spec{Offset: base, Entries: data}, Config{})
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			start := base + 50000*spacing + (1 << TimestampShift)
			if duplicates {
				start = base
			}
			dst := make([]common.ValueID, 0, n)
			expected := len(i.Search(dst[:0], start, start+1))
			b.ReportAllocs()
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				dst = i.Search(dst[:0], start, start+1)
				if len(dst) != expected {
					b.Fatal("result changed")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(expected), "matches/op")
		})
	}
	for _, distribution := range []string{"Append", "Historical"} {
		for _, persistent := range []bool{false, true} {
			name := fmt.Sprintf("Batch64/%s/Persistent=%t", distribution, persistent)
			b.Run(name, func(b *testing.B) {
				var i *Index
				var err error
				spec := Spec{Offset: base, Entries: entries}
				if persistent {
					i, err = NewStore(b.TempDir(), spec, Config{}, DurabilitySync)
				} else {
					i, err = NewMemory(spec, Config{})
				}
				if err != nil {
					b.Fatal(err)
				}
				defer i.Close()
				batch := NewBatch(64)
				old := make([]uint64, 64)
				for n := range old {
					old[n] = entries[n].Timestamp
				}
				b.ReportAllocs()
				b.ResetTimer()
				for k := 0; k < b.N; k++ {
					batch.Reset()
					for n := 0; n < 64; n++ {
						var target uint64
						if distribution == "Append" {
							target = base + uint64(100000+(k%1000+1)*64+n)*spacing
						} else {
							target = base + uint64((k*7919+n*1543)%100000)*spacing
						}
						if !batch.Append(Move(uint64(n+1), old[n], target)) {
							b.Fatal("batch")
						}
						old[n] = target
					}
					for {
						err = i.Apply(batch)
						if err != ErrBackpressure {
							break
						}
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
	b.Run("Reopen", func(b *testing.B) {
		dir := b.TempDir()
		i, err := NewStore(dir, Spec{Offset: base, Entries: entries}, Config{}, DurabilitySync)
		if err != nil {
			b.Fatal(err)
		}
		i.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for k := 0; k < b.N; k++ {
			i, err = OpenStore(dir, Config{}, DurabilitySync)
			if err != nil {
				b.Fatal(err)
			}
			if err := i.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
