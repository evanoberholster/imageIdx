package search

import (
	"context"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

func TestSearchWithMetricsCountsAndReuse(t *testing.T) {
	first := benchmarkEnumField(t, 130, "x", []uint64{1, 2, 3, 4, 5})
	second := benchmarkEnumField(t, 131, "x", []uint64{2, 4, 6})
	plan, err := Compile(AllValues(first, bitmapidx.StringValue("x")), AllValues(second, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := NewWorkspace(16)
	metrics := NewSearchMetrics(plan)
	prefix := []ImageID{99}
	got, err := plan.SearchWithMetrics(ctx, prefix, ws, metrics)
	if err != nil || !slices.Equal(got, []ImageID{99, 2, 4}) {
		t.Fatal(got, err)
	}
	if metrics.Operation != OperationSearch || !metrics.Complete || metrics.Returned != 2 || metrics.Matched != 2 || len(metrics.Stages) != 2 {
		t.Fatal(metrics)
	}
	source, filter := metrics.Stages[0], metrics.Stages[1]
	if source.Mode != StageModeSource || source.Collected != 3 || source.Passed != 3 || source.ExecutionOrder != 0 || source.Calls != 1 {
		t.Fatal(source)
	}
	if filter.Mode != StageModeFilter || filter.Input != 3 || filter.Passed != 2 || filter.Rejected() != 1 || filter.ExecutionOrder != 1 || filter.Calls != 1 {
		t.Fatal(filter)
	}
	if plain, err := plan.Search(ctx, prefix, ws); err != nil || !slices.Equal(plain, got) {
		t.Fatal(plain, err)
	}
	// Reusing the same report must reset counters and adapt to another plan.
	one, err := Compile(AllValues(first, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	got, err = one.SearchWithMetrics(ctx, nil, ws, metrics)
	if err != nil || len(got) != 5 || len(metrics.Stages) != 1 || metrics.Returned != 5 || metrics.Stages[0].Input != 0 {
		t.Fatal(got, metrics, err)
	}
}

func TestMeasuredFilteringAndRanking(t *testing.T) {
	first := benchmarkEnumField(t, 132, "x", []uint64{1, 2, 3, 4, 5})
	second := benchmarkEnumField(t, 133, "x", []uint64{2, 4, 6})
	plan, err := Compile(AllValues(first, bitmapidx.StringValue("x")), AllValues(second, bitmapidx.StringValue("x")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	metrics := NewSearchMetrics(plan)
	ids := []ImageID{6, 4, 2, 4, 1}
	got, err := plan.FilterIDsWithMetrics(ctx, nil, ids, nil, metrics)
	if err != nil || !slices.Equal(got, []ImageID{4, 2, 4}) {
		t.Fatal(got, err)
	}
	if metrics.Operation != OperationFilterIDs || metrics.Returned != 3 || metrics.Stages[0].Input != 5 || metrics.Stages[0].Passed != 4 || metrics.Stages[1].Input != 4 || metrics.Stages[1].Passed != 3 {
		t.Fatal(metrics)
	}
	lookup := func(id ImageID) (uint64, bool) { return id, true }
	ranked, err := plan.RankTimeWithMetrics(ctx, nil, nil, lookup, 4, 1, metrics)
	if err != nil || len(ranked) != 1 || ranked[0].ImageID != 4 {
		t.Fatal(ranked, err)
	}
	if metrics.Operation != OperationRankTime || !metrics.Complete || metrics.Rank.K != 1 || metrics.Rank.Candidates != 2 || metrics.Rank.Scored != 2 || metrics.Returned != 1 || metrics.Stages[1].Input != 3 || metrics.Stages[1].Passed != 2 {
		t.Fatal(metrics)
	}
	geo, err := plan.RankGeoWithMetrics(ctx, nil, nil, 0, 0, func(id ImageID) (float64, float64, bool) {
		if id == 2 {
			return math.NaN(), 0, true
		}
		return 0, 0, true
	}, 1, metrics)
	if err != nil || len(geo) != 1 || geo[0].ImageID != 4 {
		t.Fatal(geo, err)
	}
	if metrics.Operation != OperationRankGeo || metrics.Rank.Candidates != 2 || metrics.Rank.Scored != 1 || metrics.Rank.Skipped != 1 || metrics.Returned != 1 {
		t.Fatal(metrics)
	}
}

func TestSearchWithMetricsCancellationAndNil(t *testing.T) {
	plan := broadPlan(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	metrics := NewSearchMetrics(plan)
	got, err := plan.SearchWithMetrics(ctx, []ImageID{99}, nil, metrics)
	if err != context.Canceled || !slices.Equal(got, []ImageID{99}) || metrics.Complete || metrics.Returned != 0 {
		t.Fatal(got, err, metrics)
	}
	if _, err := plan.SearchWithMetrics(context.Background(), nil, nil, nil); err != ErrInvalidOptions {
		t.Fatal(err)
	}
	executor, err := NewExecutor(ExecutorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.SearchWithMetrics(context.Background(), nil, plan, nil); err != ErrInvalidOptions {
		t.Fatal(err)
	}
	plain, err := executor.Search(context.Background(), nil, plan)
	if err != nil {
		t.Fatal(err)
	}
	measured, err := executor.SearchWithMetrics(context.Background(), nil, plan, metrics)
	if err != nil || !slices.Equal(plain, measured) || metrics.Returned != uint64(len(measured)) {
		t.Fatal(measured, metrics, err)
	}
}

func TestSearchWithMetricsBufferedTimestampAndEmptyFilter(t *testing.T) {
	const quantum = uint64(1) << timeidx.TimestampShift
	field := benchmarkEnumField(t, 134, "x", []uint64{1})
	timestamps, err := timeidx.New(timeidx.Spec{Entries: []timeidx.Entry{
		{ImageID: 1, Timestamp: quantum},
		{ImageID: 3, Timestamp: 2 * quantum},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = timestamps.Close() })
	plan, err := Compile(AllValues(field, bitmapidx.StringValue("x")), During(timestamps, quantum, 3*quantum))
	if err != nil {
		t.Fatal(err)
	}
	if err := field.Add(
		bitmapidx.Entry{ImageID: 2, Value: bitmapidx.StringValue("x")},
		bitmapidx.Entry{ImageID: 3, Value: bitmapidx.StringValue("x")},
	); err != nil {
		t.Fatal(err)
	}
	metrics := NewSearchMetrics(plan)
	ctx := context.Background()
	got, err := plan.SearchWithMetrics(ctx, nil, nil, metrics)
	if err != nil || !slices.Equal(got, []ImageID{1, 3}) {
		t.Fatal(got, err)
	}
	if metrics.Stages[1].Mode != StageModeCollectIntersect || metrics.Stages[1].Input != 3 || metrics.Stages[1].Collected != 2 || metrics.Stages[1].Passed != 2 {
		t.Fatal(metrics.Stages[1])
	}
	got, err = plan.FilterIDsWithMetrics(ctx, nil, []ImageID{3, 2, 1, 3}, nil, metrics)
	if err != nil || !slices.Equal(got, []ImageID{3, 1, 3}) {
		t.Fatal(got, err)
	}
	if metrics.Stages[1].Mode != StageModeBufferedFilter || metrics.Stages[1].Collected != 2 || metrics.Stages[1].Input != 4 || metrics.Stages[1].Passed != 3 || metrics.Stages[1].Calls != 2 {
		t.Fatal(metrics.Stages[1])
	}
	_, err = plan.FilterIDsWithMetrics(ctx, nil, nil, nil, metrics)
	if err != nil || !metrics.Complete || metrics.Returned != 0 || metrics.Stages[0].ExecutionOrder != -1 {
		t.Fatal(metrics, err)
	}
}

func BenchmarkSearchMetrics(b *testing.B) {
	for _, size := range []int{100, 100_000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			plan := broadPlan(b, size)
			ctx := context.Background()
			for _, measured := range []bool{false, true} {
				name := "Search"
				if measured {
					name = "SearchWithMetrics"
				}
				b.Run(name, func(b *testing.B) {
					ws := NewWorkspace(size)
					dst := make([]ImageID, 0, size)
					metrics := NewSearchMetrics(plan)
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						var err error
						if measured {
							dst, err = plan.SearchWithMetrics(ctx, dst[:0], ws, metrics)
						} else {
							dst, err = plan.Search(ctx, dst[:0], ws)
						}
						if err != nil || len(dst) != size {
							b.Fatal(err, len(dst))
						}
					}
				})
			}
		})
	}
}
