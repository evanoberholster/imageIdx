package search

import (
	"context"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func broadPlan(tb testing.TB, size int) Plan {
	tb.Helper()
	ids := make([]uint64, size)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	a := benchmarkEnumNamespace(tb, 100, "x", ids)
	b := benchmarkEnumNamespace(tb, 101, "x", ids)
	if err := a.Compact(); err != nil {
		tb.Fatal(err)
	}
	if err := b.Compact(); err != nil {
		tb.Fatal(err)
	}
	p, err := Compile(AllValues(a, bitmapidx.EnumStringValue("x")), AllValues(b, bitmapidx.EnumStringValue("x")))
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func BenchmarkExecution(b *testing.B) {
	p := broadPlan(b, 100_000)
	b.Run("Count", func(b *testing.B) {
		ws := NewWorkspace(1024)
		p.Count(context.Background(), ws)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if count, err := p.Count(context.Background(), ws); err != nil || count != 100_000 {
				b.Fatal("count")
			}
		}
	})
	b.Run("Page1", func(b *testing.B) {
		ws := NewWorkspace(1024)
		dst := make([]ImageID, 0, 1)
		dst, _ = p.AppendPage(context.Background(), dst[:0], ws, Page{Limit: 1})
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, _ = p.AppendPage(context.Background(), dst[:0], ws, Page{Limit: 1})
		}
	})
	b.Run("FilterRanked100", func(b *testing.B) {
		ws := NewWorkspace(1024)
		ids := make([]ImageID, 100)
		for i := range ids {
			ids[i] = ImageID(100 - i)
		}
		dst := make([]Result, 0, 100)
		id := func(v ImageID) (ImageID, bool) { return v, true }
		score := func(v ImageID) uint64 { return v }
		dst, _ = p.FilterRanked(context.Background(), dst[:0], ws, ids, id, score)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, _ = p.FilterRanked(context.Background(), dst[:0], ws, ids, id, score)
		}
	})
}

func BenchmarkPersistentExecution(b *testing.B) {
	specs := []bitmapidx.NamespaceSpec{
		{Namespace: 100, Kind: common.KindBitmapEnum},
		{Namespace: 101, Kind: common.KindBitmapEnum},
	}
	s, err := bitmapidx.NewStore(b.TempDir(), specs, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	var clauses []Clause
	for _, spec := range specs {
		idx, _ := s.Lookup(spec.Namespace)
		for start := 1; start <= 100000; start += 4096 {
			entries := make([]bitmapidx.Entry, 0, 4096)
			for id := start; id < min(start+4096, 100001); id++ {
				entries = append(entries, bitmapidx.Entry{ImageID: uint64(id), Value: bitmapidx.EnumStringValue("x")})
			}
			commitBitmap(b, idx, func(bt *bitmapidx.Batch) error { return bt.Add(entries...) })
		}
		clauses = append(clauses, AllValues(idx, bitmapidx.EnumStringValue("x")))
	}
	if err := s.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	p, err := Compile(clauses...)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.Run("Count", func(b *testing.B) {
		ws := NewWorkspace(256)
		p.Count(ctx, ws)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if count, err := p.Count(ctx, ws); err != nil || count != 100000 {
				b.Fatal("count")
			}
		}
	})
	b.Run("Page1", func(b *testing.B) {
		ws := NewWorkspace(256)
		dst := make([]ImageID, 0, 1)
		p.AppendPage(ctx, dst, ws, Page{Limit: 1})
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, err = p.AppendPage(ctx, dst[:0], ws, Page{Limit: 1})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Filter100", func(b *testing.B) {
		ws := NewWorkspace(256)
		dst := make([]ImageID, 0, 100)
		ids := make([]ImageID, 100)
		for n := range ids {
			ids[n] = uint64(n + 1)
		}
		p.FilterIDs(ctx, dst, ids, ws)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, err = p.FilterIDs(ctx, dst[:0], ids, ws)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("RankTime10", func(b *testing.B) {
		ws := NewWorkspace(256)
		dst := make([]Result, 0, 10)
		lookup := func(id ImageID) (uint64, bool) { return id, true }
		p.RankTime(ctx, dst, ws, lookup, 90000, 10)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, err = p.RankTime(ctx, dst[:0], ws, lookup, 90000, 10)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPlanRefresh(b *testing.B) {
	p := broadPlan(b, 100000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := p.Refresh(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
