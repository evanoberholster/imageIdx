package search

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestEnginePlanReadsPinnedSnapshot(t *testing.T) {
	dir := t.TempDir()
	defs := schema.NewDefinitions()
	def, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: 10, Kind: schema.KindBitmapEnum,
	}, "guard", schema.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := defs.Register(def); err != nil {
		t.Fatal(err)
	}
	created, err := index.Create(dir, index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Add(index.Entry{
		ValueID: 1,
		Bitmaps: []index.BitmapValue{{Namespace: 10, Value: bitmapidx.EnumStringValue("x")}},
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	clause, err := engine.Values(10, bitmapidx.EnumStringValue("x"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Compile(schema.EntityImage, clause)
	if err != nil {
		t.Fatal(err)
	}
	if plan.guard != store {
		t.Fatal("engine plan is not bound to its store")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var seen []ImageID
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	queryDone := make(chan error, 1)
	go func() {
		queryDone <- plan.Visit(ctx, NewWorkspace(8), func(id ImageID) bool {
			seen = append(seen, id)
			once.Do(func() { close(started) })
			<-release
			return true
		})
	}()
	<-started

	// The writer must make progress while the query is still inside its
	// callback. A query read barrier would hold it here until the callback
	// returns.
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- store.Add(index.Entry{
			ValueID: 2,
			Bitmaps: []index.BitmapValue{{Namespace: 10, Value: bitmapidx.EnumStringValue("x")}},
		})
	}()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("writer blocked on an active query")
	}
	close(release)
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("query observed a newer publication: %v", seen)
	}

	got, err := plan.AppendIDs(ctx, nil, NewWorkspace(8))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("query after publication = %v", got)
	}
}
