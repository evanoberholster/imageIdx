package database

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestCommitAndApplyStats(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()

	if _, err := db.Commit(
		Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')},
		Mutation{Entity: schema.EntityImage, ValueID: 2, Projection: projection(2, 'b')},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	stats := db.Stats()
	if stats.CanonicalCommits != 1 || stats.JournalEntries != 2 || stats.JournalBytes == 0 {
		t.Fatalf("commit stats = %+v", stats)
	}
	if stats.GenerationIntentCommits == 0 || stats.IndexApplies == 0 || stats.AcknowledgmentCommits == 0 {
		t.Fatalf("apply stats = %+v", stats)
	}
	if stats.Store.Commits == 0 || stats.Store.Sets == 0 {
		t.Fatalf("store stats = %+v", stats.Store)
	}
	if stats.Index.Applies == 0 {
		t.Fatalf("index stats = %+v", stats.Index)
	}
}
