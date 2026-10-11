package storage

import (
	"errors"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

func TestCOWExtentUnionAndAllocator(t *testing.T) {
	live, err := UnionExtents([]Extent{{Ref: 2, Size: 600}}, []Extent{{Ref: 2, Size: 600}, {Ref: 4, Size: 512}})
	if err != nil || len(live) != 2 {
		t.Fatal(live, err)
	}
	if _, err := ReconstructAllocator(4096, live); err != nil {
		t.Fatal(err)
	}
	if _, err := UnionExtents(live, []Extent{{Ref: 2, Size: 512}}); err == nil {
		t.Fatal("conflict")
	}
	if _, err := ReconstructAllocator(4096, []Extent{{Ref: 2, Size: 1024}, {Ref: 3, Size: 512}}); err == nil {
		t.Fatal("overlap")
	}
	if _, err := ReconstructAllocator(4096, []Extent{{Ref: 2, Size: ^uint32(0)}}); err == nil {
		t.Fatal("overflow")
	}
	if _, err := ReconstructAllocator(1<<42, nil); err == nil {
		t.Fatal("ref overflow")
	}
}

func TestCOWExtentUnionDoesNotAliasInputs(t *testing.T) {
	a := []Extent{{Ref: 8, Size: 512}, {Ref: 2, Size: 512}, {Ref: 8, Size: 512}}
	original := slices.Clone(a)
	got, err := UnionExtents(a, []Extent{{Ref: 4, Size: 1024}, {Ref: 2, Size: 512}})
	if err != nil || !slices.Equal(got, []Extent{{Ref: 2, Size: 512}, {Ref: 4, Size: 1024}, {Ref: 8, Size: 512}}) {
		t.Fatal(got, err)
	}
	if !slices.Equal(a, original) {
		t.Fatal("mutated input")
	}
	got[0].Size = 1
	if !slices.Equal(a, original) {
		t.Fatal("aliased input")
	}
	if _, err := UnionExtents([]Extent{{Ref: 2}}); err == nil {
		t.Fatal("accepted empty extent")
	}
}

func TestCOWCommitOutcomes(t *testing.T) {
	boom := errors.New("fault")
	for _, stage := range []string{"encode", "write", "sync", "ok"} {
		payload := []byte("root")
		if stage == "encode" {
			payload = make([]byte, 512)
		}
		got, err := CommitSuperblock(SuperblockHeader{Magic: 1, Version: 1, Generation: 1}, payload, func(data []byte) error {
			if len(data) != 512 {
				t.Fatal(len(data))
			}
			if stage == "write" {
				return boom
			}
			return nil
		}, func() error {
			if stage == "sync" {
				return boom
			}
			return nil
		})
		want := CommitUncertain
		if stage == "encode" {
			want = CommitAborted
		}
		if stage == "ok" {
			want = CommitDurable
		}
		if got != want || (err == nil) != (stage == "ok") {
			t.Fatal(stage, got, err)
		}
	}
}

func TestSortedExtentMergeMatchesGeneralUnion(t *testing.T) {
	for n := 0; n < 100; n++ {
		var a, b []Extent
		for ref := 2; ref < 80; ref++ {
			e := Extent{Ref: pagealloc.Ref(ref), Size: 512}
			if (ref+n)%3 == 0 {
				a = append(a, e)
			}
			if (ref+n)%5 == 0 {
				b = append(b, e, e)
			}
		}
		originalA, originalB := slices.Clone(a), slices.Clone(b)
		got, err := UnionExtents(a, b)
		if err != nil {
			t.Fatal(err)
		}
		// Three sets deliberately use the general concatenate/sort path.
		want, err := UnionExtents(a, b, nil)
		if err != nil || !slices.Equal(got, want) {
			t.Fatal(got, want, err)
		}
		if len(got) > 0 {
			got[0].Size = 1
		}
		if !slices.Equal(a, originalA) || !slices.Equal(b, originalB) {
			t.Fatal("merge aliases inputs")
		}
	}
	for _, pair := range [][2][]Extent{
		{{{Ref: 2, Size: 512}}, {{Ref: 2, Size: 1024}}},
		{{{Ref: 2, Size: 512}, {Ref: 2, Size: 1024}}, nil},
		{nil, {{Ref: 2}}},
	} {
		if _, err := UnionExtents(pair[0], pair[1]); err == nil {
			t.Fatal("invalid sorted extents accepted", pair)
		}
	}
}
