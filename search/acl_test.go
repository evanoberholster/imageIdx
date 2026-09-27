package search

import (
	"context"
	"errors"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func aclStore(t *testing.T, includeGroups bool) *bitmapidx.Index {
	t.Helper()
	specs := []bitmapidx.NamespaceSpec{{Namespace: common.NamespaceUserACL, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType}}
	if includeGroups {
		specs = append(specs, bitmapidx.NamespaceSpec{Namespace: common.NamespaceGroupACL, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType})
	}
	specs = append(specs, bitmapidx.NamespaceSpec{Namespace: common.NamespaceTag, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType})
	store, err := bitmapidx.NewIndex(t.TempDir(), specs, bitmapidx.NamespaceConfig{}, bitmapidx.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWithACLUnionAndSearchOperations(t *testing.T) {
	store := aclStore(t, true)
	users, _ := store.Lookup(common.NamespaceUserACL)
	groups, _ := store.Lookup(common.NamespaceGroupACL)
	tags, _ := store.Lookup(common.NamespaceTag)
	user, _ := NewUserACL(42)
	otherUser, _ := NewUserACL(43)
	group7, _ := NewGroupACL(7)
	group9, _ := NewGroupACL(9)
	otherGroup, _ := NewGroupACL(10)
	for _, entry := range []struct {
		index *bitmapidx.NamespaceIndex
		id    uint64
		value bitmapidx.Value
	}{
		{users, 1, user.Value()}, {users, 3, user.Value()},
		{users, 5, otherUser.Value()},
		{groups, 2, group7.Value()}, {groups, 3, group7.Value()},
		{groups, 4, group9.Value()}, {groups, 6, otherGroup.Value()},
	} {
		commitBitmap(t, entry.index, func(b *bitmapidx.Batch) error {
			return b.Add(bitmapidx.Entry{ImageID: entry.id, Value: entry.value})
		})
	}
	for id := uint64(1); id <= 6; id++ {
		commitBitmap(t, tags, func(b *bitmapidx.Batch) error {
			return b.Add(bitmapidx.Entry{ImageID: id, Value: bitmapidx.StringValue("visible")})
		})
	}
	for _, id := range []uint64{2, 5} {
		commitBitmap(t, tags, func(b *bitmapidx.Batch) error {
			return b.Add(bitmapidx.Entry{ImageID: id, Value: bitmapidx.StringValue("select")})
		})
	}
	plan, err := CompileWithStore(store,
		AllValues(tags, bitmapidx.StringValue("visible")),
		WithACL(42, 7, 9, 7), // repeated group must not duplicate images
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := NewWorkspace(16)
	want := []ImageID{1, 2, 3, 4}
	if got, err := plan.Search(ctx, nil, ws); err != nil || !slices.Equal(got, want) {
		t.Fatal(got, err)
	}
	if count, err := plan.Count(ctx, ws); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	if found, err := plan.Exists(ctx, ws); err != nil || !found {
		t.Fatal(found, err)
	}
	if page, err := plan.AppendPage(ctx, nil, ws, Page{AfterID: 1, Limit: 2}); err != nil || !slices.Equal(page, []ImageID{2, 3}) {
		t.Fatal(page, err)
	}
	input := []ImageID{6, 4, 2, 4, 1, 3, 5}
	if got, err := plan.FilterIDs(ctx, nil, input, ws); err != nil || !slices.Equal(got, []ImageID{4, 2, 4, 1, 3}) {
		t.Fatal(got, err)
	}
	inPlace := slices.Clone(input)
	if got, err := plan.FilterIDs(ctx, inPlace[:0], inPlace, ws); err != nil || !slices.Equal(got, []ImageID{4, 2, 4, 1, 3}) {
		t.Fatal(got, err)
	}
	if got, err := plan.FilterRanked(ctx, nil, ws, input,
		func(id ImageID) (ImageID, bool) { return id, true },
		func(id ImageID) uint64 { return id },
	); err != nil || !slices.Equal(got, []Result{{4, 4}, {2, 2}, {4, 4}, {1, 1}, {3, 3}}) {
		t.Fatal(got, err)
	}
	selective, err := CompileWithStore(store,
		AllValues(tags, bitmapidx.StringValue("select")), WithACL(42, 7, 9),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := selective.Search(ctx, nil, ws); err != nil || !slices.Equal(got, []ImageID{2}) {
		t.Fatal(got, err)
	}
	metrics := NewSearchMetrics(plan)
	if got, err := plan.SearchWithMetrics(ctx, nil, ws, metrics); err != nil || !slices.Equal(got, want) || !metrics.Complete {
		t.Fatal(got, metrics, err)
	}
	aclStage := -1
	for n, stage := range metrics.Stages {
		if stage.Stage.Kind == StageACL {
			aclStage = n
		}
	}
	if aclStage < 0 || metrics.Stages[aclStage].Stage.Predicate != "any-principal" {
		t.Fatal(metrics.Stages)
	}
	// Compiled queries resolve principals added after compilation.
	group11, _ := NewGroupACL(11)
	withFuture, err := CompileWithStore(store, WithACL(42, 11))
	if err != nil {
		t.Fatal(err)
	}
	commitBitmap(t, groups, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 7, Value: group11.Value()})
	})
	commitBitmap(t, tags, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 7, Value: bitmapidx.StringValue("visible")})
	})
	if got, err := withFuture.Search(ctx, nil, ws); err != nil || !slices.Equal(got, []ImageID{1, 3, 7}) {
		t.Fatal(got, err)
	}
	refreshed, err := withFuture.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := refreshed.Search(ctx, nil, ws); err != nil || !slices.Equal(got, []ImageID{1, 3, 7}) {
		t.Fatal(got, err)
	}
	commitBitmap(t, groups, func(b *bitmapidx.Batch) error {
		return b.Remove(bitmapidx.Entry{ImageID: 7, Value: group11.Value()})
	})
	if got, err := withFuture.Search(ctx, nil, ws); err != nil || !slices.Equal(got, []ImageID{1, 3}) {
		t.Fatal(got, err)
	}
}

func TestWithACLValidationAndUserOnly(t *testing.T) {
	store := aclStore(t, false)
	users, _ := store.Lookup(common.NamespaceUserACL)
	user, _ := NewUserACL(42)
	commitBitmap(t, users, func(b *bitmapidx.Batch) error {
		return b.Add(bitmapidx.Entry{ImageID: 9, Value: user.Value()})
	})
	if _, err := Compile(WithACL(42)); !errors.Is(err, ErrInvalidClause) {
		t.Fatal(err)
	}
	for _, clause := range []Clause{WithACL(0), WithACL(42, 0), WithACL(42, 7)} {
		if _, err := CompileWithStore(store, clause); !errors.Is(err, ErrInvalidClause) {
			t.Fatal(err)
		}
	}
	plan, err := CompileWithStore(store, WithACL(42))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.Search(context.Background(), nil, nil); err != nil || !slices.Equal(got, []ImageID{9}) {
		t.Fatal(got, err)
	}
	if count, err := plan.Count(context.Background(), nil); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
