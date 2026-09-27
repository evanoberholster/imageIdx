package search

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

func TestBulkExecutionReportsStageErrors(t *testing.T) {
	namespaceIndex := benchmarkEnumNamespace(t, 120, "x", []uint64{1})
	// Compile rejects a range over an enum namespace. Construct the internal
	// stage directly to verify the bulk executor does not hide index errors.
	plan := Plan{stages: []stage{{kind: StageNamespace, namespaceIndex: namespaceIndex, namespaceKind: namespaceRange, namespaceRange: bitmapidx.Between(1, 2)}}}
	prefix := []ImageID{99}
	got, err := plan.AppendIDs(context.Background(), prefix, nil)
	if !errors.Is(err, bitmapidx.ErrInvalidRange) || !slices.Equal(got, prefix) {
		t.Fatal(got, err)
	}
	got, err = plan.AppendPage(context.Background(), prefix, nil, Page{Limit: 1})
	if !errors.Is(err, bitmapidx.ErrInvalidRange) || !slices.Equal(got, prefix) {
		t.Fatal(got, err)
	}
	got, err = plan.FilterIDs(context.Background(), prefix, []ImageID{1}, nil)
	if !errors.Is(err, bitmapidx.ErrInvalidRange) || !slices.Equal(got, prefix) {
		t.Fatal(got, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = SearchOnce(cancelled, nil, AllValues(namespaceIndex, bitmapidx.StringValue("x")))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPlanGeoEstimateIsOnlyAHint(t *testing.T) {
	tags := benchmarkEnumNamespace(t, 100, "x", nil)
	geo, err := bitmapidx.NewGeoNamespace(101, nil, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	key, _ := bitmapidx.KeyFromLatLng(37, -122)
	p, err := Compile(AllValues(tags, bitmapidx.StringValue("x")), InCells(geo, bitmapidx.DecodeS2Key(key)))
	if err != nil {
		t.Fatal(err)
	}
	commitBitmap(t, tags, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 1, Value: bitmapidx.StringValue("x")})
	})
	commitBitmap(t, geo, func(b *bitmapidx.Batch) error {
		return b.Move(bitmapidx.Move{ImageID: 1, To: int64(key), HasTo: true})
	})
	if got, err := p.AppendIDs(context.Background(), nil, nil); err != nil || !slices.Equal(got, []ImageID{1}) {
		t.Fatal(got)
	}
	if got, err := p.AppendPage(context.Background(), nil, nil, Page{Limit: 1}); err != nil || !slices.Equal(got, []ImageID{1}) {
		t.Fatal(got, err)
	}
}

func TestPagesCountVisitAndBufferedTimestamps(t *testing.T) {
	ctx := context.Background()
	p := broadPlan(t, 100000)
	ws := NewWorkspace(0)
	page, err := p.AppendPage(ctx, []ImageID{999}, ws, Page{AfterID: 65534, Limit: 3})
	if err != nil || !slices.Equal(page, []ImageID{999, 65535, 65536, 65537}) {
		t.Fatal(page, err)
	}
	if cap(ws.batch) > executionBatch || cap(ws.source) != 0 || cap(ws.scratch) != 0 {
		t.Fatal("page materialized IDs")
	}
	if _, err := p.AppendPage(ctx, nil, ws, Page{}); err == nil {
		t.Fatal("zero page limit")
	}
	if got, err := p.AppendPage(ctx, nil, ws, Page{AfterID: math.MaxUint64, Limit: 1}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	visits := 0
	if err := p.Visit(ctx, ws, func(id ImageID) bool { visits++; return false }); err != nil || visits != 1 {
		t.Fatal(visits, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	if err := p.Visit(cancelCtx, ws, func(ImageID) bool { cancel(); return true }); err != context.Canceled {
		t.Fatal(err)
	}
	// Timestamp-first requires sorting IDs; timestamp-as-filter buffers its
	// membership list once rather than rescanning it for each candidate batch.
	quantum := uint64(1) << timeidx.TimestampShift
	records := []timeidx.Entry{{ImageID: 9, Timestamp: quantum}, {ImageID: 3, Timestamp: 2 * quantum}, {ImageID: 6, Timestamp: 3 * quantum}}
	ti, err := timeidx.New(timeidx.Spec{Entries: records}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ti.Close()
	tags := benchmarkEnumNamespace(t, 110, "x", []uint64{3, 6, 9, 20})
	for _, exact := range []bool{false, true} {
		var opts []TimestampOption
		if exact {
			opts = []TimestampOption{ExactTimestamp(func(id ImageID) (uint64, bool) {
				for _, e := range records {
					if uint64(e.ImageID) == id {
						return e.Timestamp, true
					}
				}
				return 0, false
			})}
		}
		mixed, err := Compile(AllValues(tags, bitmapidx.StringValue("x")), During(ti, quantum, 4*quantum, opts...))
		if err != nil {
			t.Fatal(err)
		}
		want := []ImageID{3, 6, 9}
		if got, err := mixed.AppendIDs(ctx, nil, ws); err != nil || !slices.Equal(got, want) {
			t.Fatal(got, err)
		}
		if n, err := mixed.Count(ctx, ws); err != nil || n != 3 {
			t.Fatal(n, err)
		}
		got, err := mixed.AppendPage(ctx, nil, ws, Page{AfterID: 3, Limit: 1})
		if err != nil || !slices.Equal(got, []ImageID{6}) {
			t.Fatal(got, err)
		}
		got, err = mixed.FilterIDs(ctx, nil, []ImageID{9, 20, 3, 9}, ws)
		if err != nil || !slices.Equal(got, []ImageID{9, 3, 9}) {
			t.Fatal(got, err)
		}
	}
}

func TestFilterIDsDeltasAndRange(t *testing.T) {
	f := benchmarkEnumNamespace(t, 100, "x", []uint64{1, 3, 5})
	if err := f.Compact(); err != nil {
		t.Fatal(err)
	}
	p, err := Compile(AllValues(f, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	commitBitmap(t, f, func(b *bitmapidx.Batch) error {
		return b.Remove(bitmapidx.Entry{ImageID: 3, Value: bitmapidx.StringValue("x")})
	})
	commitBitmap(t, f, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 4, Value: bitmapidx.StringValue("x")})
	})
	got, err := p.FilterIDs(context.Background(), nil, []ImageID{3, 4, 5, 1, 4}, nil)
	if err != nil || !slices.Equal(got, []ImageID{4, 5, 1, 4}) {
		t.Fatal(got, err)
	}
	numeric, err := bitmapidx.NewNamespaceIndex(bitmapidx.NamespaceSpec{Namespace: 102, Kind: common.KindBitmapContinuous, Storage: bitmapidx.StoragePerType, Quant: bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1}}, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer numeric.Close()
	for n := 1; n <= 20; n++ {
		commitBitmap(t, numeric, func(b *bitmapidx.Batch) error {
			return b.Move(bitmapidx.Move{ImageID: uint64(n), To: int64(n), HasTo: true})
		})
	}
	for _, compact := range []bool{false, true} {
		if compact {
			if err := numeric.Compact(); err != nil {
				t.Fatal(err)
			}
		}
		rangePlan, err := Compile(NamespaceRange(numeric, bitmapidx.Between(4, 10)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := rangePlan.FilterIDs(context.Background(), nil, []ImageID{11, 8, 6, 9, 4, 3, 8}, nil)
		if err != nil || !slices.Equal(got, []ImageID{8, 6, 9, 4, 8}) {
			t.Fatal(got, err)
		}
	}
}

func TestPersistentSourceDoesNotPinCallbacks(t *testing.T) {
	s, err := bitmapidx.NewIndex(t.TempDir(), []bitmapidx.NamespaceSpec{{Namespace: 100, Kind: common.KindBitmapEnum, Storage: bitmapidx.StoragePerType}}, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idx, _ := s.Lookup(100)
	entries := make([]bitmapidx.Entry, 1000)
	for n := range entries {
		entries[n] = bitmapidx.Entry{ImageID: uint64(n + 1), Value: bitmapidx.StringValue("x")}
	}
	commitBitmap(t, idx, func(b *bitmapidx.Batch) error { return b.Add(entries...) })
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	p, err := Compile(AllValues(idx, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := p.Visit(context.Background(), nil, func(ImageID) bool {
		n++
		if n == 1 {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
		return true
	}); err != nil || n != 1000 {
		t.Fatal(n, err)
	}
}

func TestExecutorRetentionRefreshAndValidation(t *testing.T) {
	ctx := context.Background()
	ws := NewWorkspace(100000)
	ws.batch = make([]ImageID, 0, 1024)
	ws.Trim(1024)
	if ws.RetainedBytes() > 1024 || ws.Cap() != 0 {
		t.Fatal(ws.RetainedBytes())
	}
	if _, err := NewExecutor(ExecutorConfig{MaxRetainedBytes: -1}); err == nil {
		t.Fatal("negative budget")
	}
	e, err := NewExecutor(ExecutorConfig{MaxRetainedBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	e.Prewarm(2)
	p := broadPlan(t, 1000)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got, err := e.AppendPage(ctx, nil, p, Page{Limit: 2})
				if err != nil || !slices.Equal(got, []ImageID{1, 2}) {
					t.Error(got, err)
				}
				if n, err := e.Count(ctx, p); err != nil || n != 1000 {
					t.Error(n, err)
				}
			}
		}()
	}
	wg.Wait()
	a := benchmarkEnumNamespace(t, 111, "x", []uint64{1})
	b := benchmarkEnumNamespace(t, 112, "x", []uint64{1, 2})
	p, err = Compile(AllValues(a, bitmapidx.StringValue("x")), AllValues(b, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	commitBitmap(t, a, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 2, Value: bitmapidx.StringValue("x")}, bitmapidx.Entry{ImageID: 3, Value: bitmapidx.StringValue("x")})
	})
	fresh, err := p.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages()[0].Namespace != 111 || fresh.Stages()[0].Namespace != 112 {
		t.Fatal(p.Stages(), fresh.Stages())
	}
	if !fresh.Stages()[0].CanProbe || !fresh.Stages()[0].OrderedSource {
		t.Fatal(fresh.Stages())
	}
	if _, err := Compile(InCells(a, 1)); err == nil {
		t.Fatal("invalid geo accepted")
	}
	if _, err := Compile(AllValues(a)); err == nil {
		t.Fatal("empty values accepted")
	}
	ti, err := timeidx.New(timeidx.Spec{}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ti.Close()
	if _, err := Compile(During(ti, 1, 1), AllValues(nil, bitmapidx.StringValue("x"))); err == nil {
		t.Fatal("empty range skipped validation")
	}
}

func TestNativeRankingPrecision(t *testing.T) {
	p := broadPlan(t, 2)
	got, err := p.RankTime(context.Background(), nil, nil, func(id ImageID) (uint64, bool) {
		return math.MaxUint64 - id, true
	}, 0, 1)
	if err != nil || len(got) != 1 || got[0].ImageID != 2 || got[0].Score != math.MaxUint64-2 {
		t.Fatal(got, err)
	}
	for _, scale := range []float64{1, 0.000001} {
		geo, err := p.RankGeo(context.Background(), nil, nil, 0, 0, func(id ImageID) (float64, float64, bool) {
			return float64(3-id) * scale, 0, true
		}, 1)
		if err != nil || len(geo) != 1 || geo[0].ImageID != 2 {
			t.Fatal(geo, err)
		}
	}
}

func TestCandidateFilteringAndCounting(t *testing.T) {
	p := broadPlan(t, 100000)
	ws := NewWorkspace(256)
	input := []ImageID{100001, 4, 2, 4, 0, 1}
	want := []ImageID{4, 2, 4, 1}
	got, err := p.FilterIDs(context.Background(), []ImageID{99}, input, ws)
	if err != nil || !slices.Equal(got, append([]ImageID{99}, want...)) {
		t.Fatal(got, err)
	}
	got, err = p.FilterIDs(context.Background(), input[:0], input, ws)
	if err != nil || !slices.Equal(got, want) {
		t.Fatal(got, err)
	}
	if n, err := p.Count(context.Background(), ws); err != nil || n != 100000 {
		t.Fatal(n, err)
	}
	if ok, err := p.Exists(context.Background(), ws); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if cap(ws.batch) > executionBatch {
		t.Fatal("unbounded candidate scratch")
	}
	ranked := []fakeANN{{node: 4, dist: 90000}, {node: 4, dist: 100000}, {node: 100001, dist: 3}, {node: 2, dist: 70000}}
	out, err := p.FilterRanked(context.Background(), nil, ws, ranked, func(r fakeANN) (ImageID, bool) { return uint64(r.node), true }, func(r fakeANN) uint64 { return uint64(r.dist) })
	if err != nil || !slices.Equal(out, []Result{{4, 90000}, {4, 100000}, {2, 70000}}) {
		t.Fatal(out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := p.FilterIDs(ctx, []ImageID{99}, want, ws); err == nil || !slices.Equal(got, []ImageID{99}) {
		t.Fatal(got, err)
	}
}
