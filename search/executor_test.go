package search

import (
	"context"
	"slices"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func TestExecutorCountAndAppendIDs(t *testing.T) {
	tags := enumNamespaceForSearch(t, schema.NamespaceTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.EnumStringValue("favorite")))
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(ExecutorConfig{WorkspaceCapacity: 16})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := executor.Count(context.Background(), plan); err != nil || got != 3 {
		t.Fatalf("Count = %d, %v; want 3, nil", got, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Count(cancelled, plan); err == nil {
		t.Fatal("cancelled Count succeeded")
	}
	if got, err := executor.AppendIDs(context.Background(), nil, plan); err != nil || !slices.Equal(got, []ImageID{1, 2, 3}) {
		t.Fatalf("AppendIDs = %v, %v, want [1 2 3]", got, err)
	}
}
