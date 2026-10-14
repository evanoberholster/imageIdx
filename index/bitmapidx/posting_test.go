package bitmapidx

import (
	"slices"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func basePosting(t *testing.T, index *Index, key NamespaceKey) *posting {
	t.Helper()
	p, ok := index.view.Load().base.get(key)
	if !ok {
		t.Fatalf("no base posting for %+v", key)
	}
	return p
}

func TestIntersectPostingsEmpty(t *testing.T) {
	if out := intersectPostings(nil); !out.IsEmpty() {
		t.Fatalf("intersectPostings(nil) = %v; want empty", out.AppendTo(nil))
	}
	if count := intersectPostingsCardinality(nil); count != 0 {
		t.Fatalf("intersectPostingsCardinality(nil) = %d; want 0", count)
	}
}

func TestTinyPostingTier(t *testing.T) {
	rare := hashCanonical(schema.NamespaceTag, schema.KindBitmapEnum, []byte("rare"))
	index := mustNamespace(t, enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: EnumStringValue("rare")},
			Entry{ImageID: 2, Value: EnumStringValue("rare")},
			Entry{ImageID: 3, Value: EnumStringValue("rare")},
		)
	})
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if p := basePosting(t, index, rare); !p.isTiny() || p.cardinality() != 3 {
		t.Fatalf("rare posting = tiny:%v card:%d, want tiny 3", p.isTiny(), p.cardinality())
	}
	if p := basePosting(t, index, rare); !p.contains(2) || p.contains(9) {
		t.Fatal("tiny contains mismatch")
	}
	if ids := appendIDs(t, index, EnumStringValue("rare")); !slices.Equal(ids, []schema.ImageID{1, 2, 3}) {
		t.Fatalf("ValueIDs = %v", ids)
	}

	// Promotion: a fifth member exceeds the inline capacity.
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 4, Value: EnumStringValue("rare")},
			Entry{ImageID: 5, Value: EnumStringValue("rare")},
		)
	})
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if p := basePosting(t, index, rare); p.isTiny() || p.cardinality() != 5 {
		t.Fatalf("promoted posting = tiny:%v card:%d, want bitmap 5", p.isTiny(), p.cardinality())
	}

	// Demotion: collapsing back to three members returns to the inline form.
	commit(t, index, func(b *Batch) error {
		return b.Remove(
			Entry{ImageID: 1, Value: EnumStringValue("rare")},
			Entry{ImageID: 2, Value: EnumStringValue("rare")},
		)
	})
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if p := basePosting(t, index, rare); !p.isTiny() || p.cardinality() != 3 {
		t.Fatalf("demoted posting = tiny:%v card:%d, want tiny 3", p.isTiny(), p.cardinality())
	}
	if ids := appendIDs(t, index, EnumStringValue("rare")); !slices.Equal(ids, []schema.ImageID{3, 4, 5}) {
		t.Fatalf("ValueIDs after demotion = %v", ids)
	}

	// Tiny-to-tiny intersection and count avoid the roaring kernel.
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 3, Value: EnumStringValue("overlap")},
			Entry{ImageID: 4, Value: EnumStringValue("overlap")},
		)
	})
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	match, err := index.MatchValues(EnumStringValue("rare"), EnumStringValue("overlap"))
	if err != nil {
		t.Fatalf("MatchValues: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []schema.ImageID{3, 4}) {
		t.Fatalf("tiny intersection = %v, want [3 4]", got)
	}
	count, err := index.CountValues(EnumStringValue("rare"), EnumStringValue("overlap"))
	if err != nil || count != 2 {
		t.Fatalf("CountValues = %d, %v", count, err)
	}
}

func TestTinyPostingTierKinds(t *testing.T) {
	// Continuous: two rare grid values, each with one image, union inline.
	focals := mustNamespace(t, continuousNamespace(testFocalLength), NamespaceConfig{})
	commit(t, focals, func(b *Batch) error {
		return b.Move(
			Move{ImageID: 1, To: 24},
			Move{ImageID: 2, To: 50},
		)
	})
	if err := focals.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	match, err := focals.Range(Between(0, 100))
	if err != nil || !slices.Equal(match.AppendTo(nil), []schema.ImageID{1, 2}) {
		t.Fatalf("Range = %v, %v", match.AppendTo(nil), err)
	}

	// Text: a token with a single member stays inline and matches.
	titles := mustNamespace(t, textNamespace(schema.NamespaceTag), NamespaceConfig{})
	commit(t, titles, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 7, After: []string{"golden"}})
	})
	if err := titles.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	token := hashCanonical(schema.NamespaceTag, schema.KindBitmapString, []byte("golden"))
	if p := basePosting(t, titles, token); !p.isTiny() {
		t.Fatal("text posting not tiny")
	}
	match, err = titles.MatchText("golden")
	if err != nil || !slices.Equal(match.AppendTo(nil), []schema.ImageID{7}) {
		t.Fatalf("MatchText = %v, %v", match.AppendTo(nil), err)
	}
}
