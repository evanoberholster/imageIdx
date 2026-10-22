package search

import (
	"testing"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func TestPlanCostOrdersNarrowerCandidateFirst(t *testing.T) {
	wide := enumNamespaceForSearch(t, 111, "wide", []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	narrow := enumNamespaceForSearch(t, 112, "narrow", []uint64{2, 4})
	plan, err := Compile(
		AllValues(wide, bitmapidx.EnumStringValue("wide")),
		AllValues(narrow, bitmapidx.EnumStringValue("narrow")),
	)
	if err != nil {
		t.Fatal(err)
	}
	stages := plan.Stages()
	if len(stages) != 2 || stages[0].Namespace != 112 {
		t.Fatalf("stages = %+v", stages)
	}
	if stages[0].Cost > stages[1].Cost {
		t.Fatalf("cost order = %+v", stages)
	}
	if got, err := plan.AppendIDs(t.Context(), nil, NewWorkspace(8)); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("matches = %v", got)
	}
}

func TestStageCostFamilyWeights(t *testing.T) {
	enum := stage{kind: StageNamespace, estimate: 100, namespaceKind: namespaceValues}
	bsi := stage{kind: StageNamespace, estimate: 100, namespaceKind: namespaceBSIRange}
	geo := stage{kind: StageGeo, estimate: 100}
	exact := stage{kind: StageTimestamp, estimate: 100, tsExact: func(uint64) (uint64, bool) { return 0, false }}
	lossy := stage{kind: StageTimestamp, estimate: 100}
	if stageCost(&enum) >= stageCost(&bsi) {
		t.Fatalf("BSI cost should exceed enum: %d %d", stageCost(&enum), stageCost(&bsi))
	}
	if stageCost(&enum) >= stageCost(&geo) {
		t.Fatalf("geo cost should exceed enum: %d %d", stageCost(&enum), stageCost(&geo))
	}
	if stageCost(&exact) <= stageCost(&lossy) {
		t.Fatalf("exact timestamp cost should exceed lossy: %d %d", stageCost(&exact), stageCost(&lossy))
	}
}
