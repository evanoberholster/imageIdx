package bitmapidx

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
)

const testFocalLength common.Namespace = 100

func enumNamespace(t common.Namespace) NamespaceSpec {
	return NamespaceSpec{Namespace: t, Kind: common.KindBitmapEnum, Normalizer: CodecSpec{Code: NormalizerIdentity, Version: 1}}
}

func boolNamespace(t common.Namespace) NamespaceSpec {
	return NamespaceSpec{Namespace: t, Kind: common.KindBitmapBool}
}

func continuousNamespace(t common.Namespace) NamespaceSpec {
	return NamespaceSpec{Namespace: t, Kind: common.KindBitmapContinuous, Quant: Quantizer{Code: QuantIdentity, Version: 1, Scale: 1}}
}

func textNamespace(t common.Namespace) NamespaceSpec {
	return NamespaceSpec{Namespace: t, Kind: common.KindBitmapString, Analyzer: CodecSpec{Code: textanalyzer.CodeLatin, Version: textanalyzer.Version}}
}

func mustNamespace(t *testing.T, namespace NamespaceSpec, config NamespaceConfig) *Index {
	t.Helper()
	index, err := NewMemory(namespace, config)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func appendIDs(t *testing.T, index *Index, value Value) []common.ImageID {
	t.Helper()
	ids, err := index.ValueIDs(nil, value)
	if err != nil {
		t.Fatalf("ValueIDs: %v", err)
	}
	return ids
}

func matchIDs(t *testing.T, index *Index, values ...Value) []common.ImageID {
	t.Helper()
	match, err := index.MatchValues(values...)
	if err != nil {
		t.Fatalf("MatchValues: %v", err)
	}
	return match.AppendTo(nil)
}

func TestHashDomainSeparation(t *testing.T) {
	base := hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("x"))
	if base != hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("x")) {
		t.Fatal("hash is not deterministic")
	}
	if base == hashCanonical(common.NamespaceAlbum, common.KindBitmapEnum, []byte("x")) {
		t.Fatal("type is not domain-separated")
	}
	if base == hashCanonical(common.NamespaceTag, common.KindBitmapString, []byte("x")) {
		t.Fatal("kind is not domain-separated")
	}
	if base == hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("y")) {
		t.Fatal("value is not domain-separated")
	}
}

func TestNamespaceSpecSchemaRoundTrip(t *testing.T) {
	specs := []NamespaceSpec{enumNamespace(common.NamespaceTag), textNamespace(common.NamespaceTag + 1)}
	data := encodeNamespaceSpecs(specs)
	restored, err := decodeNamespaceSpecs(data)
	if err != nil {
		t.Fatalf("decodeNamespaceSpecs: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("restored %d namespaces, want 2", len(restored))
	}
	var tag NamespaceSpec
	found := false
	for _, spec := range restored {
		if spec.Namespace == common.NamespaceTag {
			tag, found = spec, true
			break
		}
	}
	if !found || tag.Kind != common.KindBitmapEnum {
		t.Fatalf("restored namespace = %+v, ok=%v", tag, found)
	}

	enum := enumNamespace(common.NamespaceTag)
	def, err := enum.NamespaceDef("tag")
	if err != nil {
		t.Fatalf("NamespaceDef: %v", err)
	}
	if def.Namespace != common.NamespaceTag || def.Name != "tag" || def.Kind != common.KindBitmapEnum {
		t.Fatalf("NamespaceDef = %+v", def)
	}
	if _, err := enum.NamespaceDef(""); !errors.Is(err, common.ErrNamespaceName) {
		t.Fatalf("empty schema name error = %v, want ErrNamespaceName", err)
	}
	back, err := NamespaceSpecFromSchemaDef(def)
	if err != nil {
		t.Fatalf("NamespaceSpecFromSchemaDef: %v", err)
	}
	if back.Namespace != enum.Namespace || back.Kind != enum.Kind ||
		back.Normalizer.Code != enum.Normalizer.Code || back.Normalizer.Version != enum.Normalizer.Version ||
		!slices.Equal(back.Normalizer.Params, enum.Normalizer.Params) {
		t.Fatalf("round trip = %+v, want %+v", back, enum)
	}
	mismatchedBitmapKind := def
	mismatchedBitmapKind.Kind = common.KindBitmapBool
	if _, err := NamespaceSpecFromSchemaDef(mismatchedBitmapKind); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("mismatched bitmap kind error = %v, want ErrNamespaceMismatch", err)
	}
	wrongKind := common.NamespaceDef{Namespace: common.NamespaceTag, Name: "tag", Kind: common.KindVector, Config: def.Config}
	if _, err := NamespaceSpecFromSchemaDef(wrongKind); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("wrong kind error = %v, want ErrNamespaceMismatch", err)
	}

	if err := validateNamespaceSpecs([]NamespaceSpec{enumNamespace(common.NamespaceTag), boolNamespace(common.NamespaceTag)}); !errors.Is(err, common.ErrNamespaceDuplicate) {
		t.Fatalf("duplicate namespace error = %v, want ErrNamespaceDuplicate", err)
	}
	bad := textNamespace(common.NamespaceTag + 2)
	bad.Analyzer.Code = 99
	if err := validateNamespaceSpecs([]NamespaceSpec{bad}); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("bad codec error = %v, want ErrInvalidNamespace", err)
	}
}

func TestBitmapKindWireCodes(t *testing.T) {
	for _, tc := range []struct {
		kind common.NamespaceKind
		code byte
	}{
		{common.KindBitmapEnum, 1},
		{common.KindBitmapBool, 2},
		{common.KindBitmapString, 3},
		{common.KindBitmapContinuous, 4},
	} {
		if got := bitmapKindCode(tc.kind); got != tc.code {
			t.Errorf("bitmapKindCode(%v) = %d, want %d", tc.kind, got, tc.code)
		}
		if got := bitmapKindFromCode(tc.code); got != tc.kind {
			t.Errorf("bitmapKindFromCode(%d) = %v, want %v", tc.code, got, tc.kind)
		}
	}
	if got := bitmapKindFromCode(0); got != 0 {
		t.Fatalf("bitmapKindFromCode(0) = %v, want invalid kind", got)
	}
	// Geography moved to geoidx: the bitmap kind no longer has a wire code.
	if got := bitmapKindCode(common.KindBitmapGeo); got != 0 {
		t.Fatalf("bitmapKindCode(geo) = %d, want 0", got)
	}
}

func TestLegacyStorageByteStillDecodes(t *testing.T) {
	wire := appendNamespace(nil, enumNamespace(common.NamespaceTag))
	if wire[3] != 2 {
		t.Fatalf("storage byte = %d, want 2", wire[3])
	}
	wire[3] = 1
	if _, _, err := decodeNamespace(wire, 0); err != nil {
		t.Fatalf("decode old storage byte: %v", err)
	}
	wire[3] = 3
	if _, _, err := decodeNamespace(wire, 0); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("invalid storage byte error = %v, want ErrInvalidNamespace", err)
	}
}

func TestHashCollisionSlots(t *testing.T) {
	original := hashCanonical
	hashCanonical = func(t common.Namespace, _ common.NamespaceKind, _ []byte) NamespaceKey {
		return NamespaceKey{Namespace: t, Hash: 0xc0111ded}
	}
	t.Cleanup(func() { hashCanonical = original })

	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: EnumStringValue("alpha")},
			Entry{ImageID: 2, Value: EnumStringValue("beta")},
			Entry{ImageID: 3, Value: EnumStringValue("gamma")},
		)
	})
	alphaKey, ok, err := index.KeyForValue(EnumStringValue("alpha"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue(alpha): ok=%v err=%v", ok, err)
	}
	betaKey, ok, err := index.KeyForValue(EnumStringValue("beta"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue(beta): ok=%v err=%v", ok, err)
	}
	gammaKey, ok, err := index.KeyForValue(EnumStringValue("gamma"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue(gamma): ok=%v err=%v", ok, err)
	}
	if alphaKey.Hash != betaKey.Hash || betaKey.Hash != gammaKey.Hash {
		t.Fatal("test setup: injected hashes differ")
	}
	if alphaKey.Slot == betaKey.Slot || betaKey.Slot == gammaKey.Slot || alphaKey.Slot == gammaKey.Slot {
		t.Fatalf("colliding values share slots: %d %d %d", alphaKey.Slot, betaKey.Slot, gammaKey.Slot)
	}
	for _, check := range []struct {
		value Value
		want  []common.ImageID
	}{
		{EnumStringValue("alpha"), []common.ImageID{1}},
		{EnumStringValue("beta"), []common.ImageID{2}},
		{EnumStringValue("gamma"), []common.ImageID{3}},
	} {
		if got := appendIDs(t, index, check.value); !slices.Equal(got, check.want) {
			t.Fatalf("%s = %v, want %v", check.value.Bytes, got, check.want)
		}
	}
	if present, err := index.Contains(1, EnumStringValue("beta")); err != nil || present {
		t.Fatalf("alpha matched beta: present=%v err=%v", present, err)
	}

	// Removing one colliding value must not disturb the others.
	commit(t, index, func(b *Batch) error {
		return b.Remove(Entry{ImageID: 2, Value: EnumStringValue("beta")})
	})
	if got := appendIDs(t, index, EnumStringValue("alpha")); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatalf("alpha after beta removal = %v", got)
	}
	if got := appendIDs(t, index, EnumStringValue("gamma")); !slices.Equal(got, []common.ImageID{3}) {
		t.Fatalf("gamma after beta removal = %v", got)
	}
	if got := appendIDs(t, index, EnumStringValue("beta")); len(got) != 0 {
		t.Fatalf("beta after removal = %v", got)
	}
	if value, ok := index.LookupValue(nil, gammaKey); !ok || string(value) != "gamma" {
		t.Fatalf("LookupValue(gamma) = %q, %v", value, ok)
	}

	// Vacuum drops the dead value and its slot while preserving live keys.
	if err := index.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if _, ok, err := index.KeyForValue(EnumStringValue("beta")); err != nil || ok {
		t.Fatalf("beta after vacuum: ok=%v err=%v", ok, err)
	}
	if got := appendIDs(t, index, EnumStringValue("alpha")); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatalf("alpha after vacuum = %v", got)
	}
	commit(t, index, func(b *Batch) error {
		return b.Add(Entry{ImageID: 4, Value: EnumStringValue("beta")})
	})
	if got := appendIDs(t, index, EnumStringValue("beta")); !slices.Equal(got, []common.ImageID{4}) {
		t.Fatalf("beta after re-add = %v", got)
	}
	if got := appendIDs(t, index, EnumStringValue("gamma")); !slices.Equal(got, []common.ImageID{3}) {
		t.Fatalf("gamma after re-add = %v", got)
	}
	var values []string
	index.Iterate(func(_ NamespaceKey, value []byte) bool {
		values = append(values, string(value))
		return true
	})
	slices.Sort(values)
	if !slices.Equal(values, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("iterated values = %v", values)
	}
}

func TestNamespaceIndexEnumLifecycle(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if index.Spec().Kind != common.KindBitmapEnum {
		t.Fatalf("kind = %v", index.Spec().Kind)
	}
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: EnumStringValue("Paris")},
			Entry{ImageID: 2, Value: EnumStringValue("Paris")},
			Entry{ImageID: 3, Value: EnumStringValue("Rome")},
		)
	})
	if got := matchIDs(t, index, EnumStringValue("Paris")); !slices.Equal(got, []common.ImageID{1, 2}) {
		t.Fatalf("Paris = %v", got)
	}
	if got := matchIDs(t, index, EnumStringValue("Paris"), EnumStringValue("Rome")); len(got) != 0 {
		t.Fatalf("Paris AND Rome = %v, want empty", got)
	}
	if present, err := index.Contains(1, EnumStringValue("Paris")); err != nil || !present {
		t.Fatalf("Contains(1, Paris) = %v, %v", present, err)
	}
	if present, err := index.Contains(3, EnumStringValue("Paris")); err != nil || present {
		t.Fatalf("Contains(3, Paris) = %v, %v", present, err)
	}

	// Remove wins over Add within one batch.
	commit(t, index, func(b *Batch) error {
		if err := b.Add(Entry{ImageID: 4, Value: EnumStringValue("Paris")}); err != nil {
			return err
		}
		return b.Remove(Entry{ImageID: 4, Value: EnumStringValue("Paris")})
	})
	if got := matchIDs(t, index, EnumStringValue("Paris")); !slices.Equal(got, []common.ImageID{1, 2}) {
		t.Fatalf("Paris after remove-wins = %v", got)
	}

	commit(t, index, func(b *Batch) error {
		return b.Remove(Entry{ImageID: 1, Value: EnumStringValue("Paris")})
	})
	if got := matchIDs(t, index, EnumStringValue("Paris")); !slices.Equal(got, []common.ImageID{2}) {
		t.Fatalf("Paris after remove = %v", got)
	}

	count, err := index.CountValues(EnumStringValue("Paris"))
	if err != nil || count != 1 {
		t.Fatalf("CountValues = %d, %v", count, err)
	}
	filtered, err := index.FilterValues(nil, []common.ImageID{1, 2, 3, 4}, EnumStringValue("Paris"))
	if err != nil || !slices.Equal(filtered, []common.ImageID{2}) {
		t.Fatalf("FilterValues = %v, %v", filtered, err)
	}
}

func TestNamespaceIndexBoolExplicitFalse(t *testing.T) {
	index := mustNamespace(t, boolNamespace(common.NamespaceFlag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: BoolValue(false)},
			Entry{ImageID: 2, Value: BoolValue(true)},
		)
	})
	if present, _ := index.Contains(1, BoolValue(false)); !present {
		t.Fatal("explicit false is not a member")
	}
	if present, _ := index.Contains(3, BoolValue(false)); present {
		t.Fatal("absent image reported as false member")
	}
	falseIDs := appendIDs(t, index, BoolValue(false))
	if !slices.Equal(falseIDs, []common.ImageID{1}) {
		t.Fatalf("false IDs = %v", falseIDs)
	}
	trueIDs := appendIDs(t, index, BoolValue(true))
	if !slices.Equal(trueIDs, []common.ImageID{2}) {
		t.Fatalf("true IDs = %v", trueIDs)
	}
}

func TestNamespaceIndexContinuousRange(t *testing.T) {
	index := mustNamespace(t, continuousNamespace(testFocalLength), NamespaceConfig{})
	values := []int64{math.MinInt64, -100, -1, 0, 1, 50, 64, 70, 100, math.MaxInt64}
	for n, value := range values {
		commit(t, index, func(b *Batch) error {
			return b.Add(Entry{ImageID: common.ImageID(n + 1), Value: IntValue(value)})
		})
	}
	check := func(bounds NumericRange, want []int64) {
		t.Helper()
		match, err := index.Range(bounds)
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		got := match.AppendTo(nil)
		ids := make([]common.ImageID, 0, len(want))
		for _, value := range want {
			ids = append(ids, common.ImageID(slices.Index(values, value)+1))
		}
		if !slices.Equal(got, ids) {
			t.Fatalf("Range %+v = %v, want %v", bounds, got, ids)
		}
	}
	check(Between(50, 100), []int64{50, 64, 70})
	check(NumericRange{Lo: 50, Hi: 100, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}, []int64{50, 64, 70, 100})
	check(AtLeast(700), []int64{math.MaxInt64})
	check(AtMost(-100), []int64{math.MinInt64, -100})
	check(Between(64, 64), nil)

	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	estimate, err := index.EstimateRange(Between(0, 100))
	if err != nil || estimate != 5 {
		t.Fatalf("EstimateRange = %d, %v", estimate, err)
	}
	if _, err := index.MatchValues(IntValue(64)); err != nil {
		t.Fatalf("exact continuous match: %v", err)
	}
	if _, err := index.Range(Between(0, 1)); err != nil {
		t.Fatalf("Range: %v", err)
	}

	enum := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if _, err := enum.Range(Between(0, 1)); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("Range on enum = %v, want ErrInvalidRange", err)
	}
	if _, err := enum.EstimateRange(Between(0, 1)); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("EstimateRange on enum = %v, want ErrInvalidRange", err)
	}
}

func TestNamespaceIndexMove(t *testing.T) {
	index := mustNamespace(t, continuousNamespace(testFocalLength), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(Entry{ImageID: 1, Value: IntValue(24)})
	})
	commit(t, index, func(b *Batch) error {
		return b.Move(Move{ImageID: 1, From: 24, To: 35})
	})
	if ids := appendIDs(t, index, IntValue(24)); len(ids) != 0 {
		t.Fatalf("old value still present: %v", ids)
	}
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []common.ImageID{1}) {
		t.Fatalf("new value = %v", ids)
	}
	// Same-value move is a no-op, not add/remove cancellation.
	commit(t, index, func(b *Batch) error {
		return b.Move(Move{ImageID: 1, From: 35, To: 35})
	})
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []common.ImageID{1}) {
		t.Fatalf("same-value move removed membership: %v", ids)
	}
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Move(
			Move{ImageID: 1, From: 35, To: 50},
			Move{ImageID: 1, From: 50, To: 85},
		); !errors.Is(err, ErrInvalidMutation) {
			t.Fatalf("duplicate move error = %v, want ErrInvalidMutation", err)
		}
	}
}

func TestNamespaceIndexTextReplace(t *testing.T) {
	index := mustNamespace(t, textNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(
			TextReplacement{ImageID: 1, After: []string{"Golden Gate Bridge"}},
			TextReplacement{ImageID: 2, After: []string{"Golden Gate Park"}},
		)
	})
	match, err := index.MatchText("golden gate")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []common.ImageID{1, 2}) {
		t.Fatalf("golden gate = %v", got)
	}
	match, _ = index.MatchText("bridge")
	if got := match.AppendTo(nil); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatalf("bridge = %v", got)
	}
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 1, Before: []string{"Golden Gate Bridge"}, After: []string{"Golden Gate Park"}})
	})
	match, _ = index.MatchText("bridge")
	if got := match.AppendTo(nil); len(got) != 0 {
		t.Fatalf("bridge after replace = %v", got)
	}
	// Token retained in both old and new text must not be removed.
	match, _ = index.MatchText("golden")
	if got := match.AppendTo(nil); !slices.Equal(got, []common.ImageID{1, 2}) {
		t.Fatalf("golden after overlapping replace = %v", got)
	}
	// Zero-token queries match nothing.
	match, _ = index.MatchText("!!!")
	if got := match.AppendTo(nil); len(got) != 0 {
		t.Fatalf("empty token query = %v", got)
	}
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Add(Entry{ImageID: 3, Value: Value{Kind: common.KindBitmapString, Bytes: []byte("x")}}); !errors.Is(err, ErrUnsupportedOperation) {
			t.Fatalf("text Add error = %v, want ErrUnsupportedOperation", err)
		}
	}
}

func TestNamespaceIndexDeltaRotationAndCompaction(t *testing.T) {
	config := NamespaceConfig{ActiveTarget: 3, HardDeltaLimit: 8, MaxDeltaValues: 2}
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), config)
	commit(t, index, func(b *Batch) error {
		return b.Add(Entry{ImageID: 1, Value: EnumStringValue("a")})
	})
	// Cross ActiveTarget to rotate into Frozen and schedule compaction.
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 2, Value: EnumStringValue("a")},
			Entry{ImageID: 3, Value: EnumStringValue("a")},
			Entry{ImageID: 4, Value: EnumStringValue("a")},
		)
	})
	if err := index.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := matchIDs(t, index, EnumStringValue("a")); !slices.Equal(got, []common.ImageID{1, 2, 3, 4}) {
		t.Fatalf("after rotation = %v", got)
	}
	// A batch above HardDeltaLimit folds the base directly.
	large := make([]Entry, 0, 12)
	for id := common.ImageID(10); id < 22; id++ {
		large = append(large, Entry{ImageID: id, Value: EnumStringValue("b")})
	}
	commit(t, index, func(b *Batch) error { return b.Add(large...) })
	if got := matchIDs(t, index, EnumStringValue("b")); len(got) != 12 {
		t.Fatalf("large add = %v", got)
	}
	if got := matchIDs(t, index, EnumStringValue("a")); !slices.Equal(got, []common.ImageID{1, 2, 3, 4}) {
		t.Fatalf("base preserved = %v", got)
	}

	// Delta beyond MaxDeltaValues folds too.
	for value := 0; value < 4; value++ {
		commit(t, index, func(b *Batch) error {
			return b.Add(Entry{ImageID: common.ImageID(100 + value), Value: EnumStringValue(string(rune('p' + value)))})
		})
	}
	if got := matchIDs(t, index, EnumStringValue("p")); !slices.Equal(got, []common.ImageID{100}) {
		t.Fatalf("folded value = %v", got)
	}
}

func TestNamespaceIndexVacuumDropsDeadValues(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: EnumStringValue("dead")},
			Entry{ImageID: 2, Value: EnumStringValue("live")},
		)
	})
	commit(t, index, func(b *Batch) error {
		return b.Remove(Entry{ImageID: 1, Value: EnumStringValue("dead")})
	})
	if _, ok, err := index.KeyForValue(EnumStringValue("dead")); err != nil || !ok {
		t.Fatalf("dead before vacuum: ok=%v err=%v", ok, err)
	}
	if err := index.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if _, ok, err := index.KeyForValue(EnumStringValue("dead")); err != nil || ok {
		t.Fatalf("dead after vacuum: ok=%v err=%v", ok, err)
	}
	if got := matchIDs(t, index, EnumStringValue("live")); !slices.Equal(got, []common.ImageID{2}) {
		t.Fatalf("live after vacuum = %v", got)
	}
	var visited []string
	index.Iterate(func(_ NamespaceKey, value []byte) bool {
		visited = append(visited, string(value))
		return true
	})
	if !slices.Equal(visited, []string{"live"}) {
		t.Fatalf("iterated values = %v", visited)
	}
}

func TestNamespaceIndexPrefixAndLookup(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(
			Entry{ImageID: 1, Value: EnumStringValue("Trekking")},
			Entry{ImageID: 2, Value: EnumStringValue("Travel")},
			Entry{ImageID: 3, Value: EnumStringValue("Family")},
		)
	})
	key, ok, err := index.KeyForValue(EnumStringValue("Travel"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue: ok=%v err=%v", ok, err)
	}
	if value, ok := index.LookupValue(nil, key); !ok || string(value) != "Travel" {
		t.Fatalf("LookupValue = %q, %v", value, ok)
	}
	var prefixed []string
	index.Prefix([]byte("Tr"), func(_ NamespaceKey, value []byte) bool {
		prefixed = append(prefixed, string(value))
		return true
	})
	if !slices.Equal(prefixed, []string{"Travel", "Trekking"}) {
		t.Fatalf("prefix = %v", prefixed)
	}
	// Removal-only batches never create dictionary entries.
	commit(t, index, func(b *Batch) error {
		return b.Remove(Entry{ImageID: 9, Value: EnumStringValue("Ghost")})
	})
	if _, ok, _ := index.KeyForValue(EnumStringValue("Ghost")); ok {
		t.Fatal("removal-only value was created")
	}
}

func TestNamespaceIndexValidation(t *testing.T) {
	if _, err := NewMemory(NamespaceSpec{Namespace: 0, Kind: common.KindBitmapEnum}, NamespaceConfig{}); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("zero type error = %v", err)
	}
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Add(Entry{ImageID: 0, Value: EnumStringValue("x")}); !errors.Is(err, ErrInvalidImageID) {
			t.Fatalf("zero id error = %v", err)
		}
	}
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Add(Entry{ImageID: 1, Value: BoolValue(true)}); !errors.Is(err, ErrValueKind) {
			t.Fatalf("wrong kind error = %v", err)
		}
	}
}

func TestNamespaceIndexConcurrentReadersWriters(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{ActiveTarget: 64, HardDeltaLimit: 128})
	var wait sync.WaitGroup
	stop := make(chan struct{})
	for reader := 0; reader < 4; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = index.MatchValues(EnumStringValue("a"))
				_ = index.ContainsKey(1, hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("a")))
				_, _, _ = index.KeyForValue(EnumStringValue("a"))
			}
		}()
	}
	for batch := 0; batch < 200; batch++ {
		id := common.ImageID(batch + 1)
		commit(t, index, func(b *Batch) error { return b.Add(Entry{ImageID: id, Value: EnumStringValue("a")}) })
		if batch%3 == 0 {
			commit(t, index, func(b *Batch) error { return b.Remove(Entry{ImageID: id, Value: EnumStringValue("a")}) })
		}
	}
	close(stop)
	wait.Wait()
}

func TestNamespaceIndexTextAnalyzerPipeline(t *testing.T) {
	index := mustNamespace(t, textNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 1, After: []string{"Café au lait!"}})
	})
	match, err := index.MatchText("CAFE lait")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatalf("accent-folded match = %v", got)
	}
}

func TestNumericRangeEmpty(t *testing.T) {
	if !Between(5, 4).empty() {
		t.Fatal("Between(5,4) should be empty")
	}
	if !Between(5, 5).empty() {
		t.Fatal("Between(5,5) is empty: half-open equal bounds")
	}
	if !(NumericRange{Lo: 5, Hi: 5, HasLo: true, HasHi: true}).empty() {
		t.Fatal("exclusive equal bounds should be empty")
	}
	if (NumericRange{Lo: 5, Hi: 5, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}).empty() {
		t.Fatal("inclusive equal bounds should match one value")
	}
}

func TestNamespaceIndexMoveToEmpty(t *testing.T) {
	index := mustNamespace(t, continuousNamespace(testFocalLength), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.Add(Entry{ImageID: 1, Value: IntValue(24)})
	})
	commit(t, index, func(b *Batch) error {
		return b.Move(Move{ImageID: 1, From: 24, To: 0})
	})
	if ids := appendIDs(t, index, IntValue(24)); len(ids) != 0 {
		t.Fatalf("value after remove-move = %v", ids)
	}
	commit(t, index, func(b *Batch) error {
		return b.Move(Move{ImageID: 2, To: 35})
	})
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []common.ImageID{2}) {
		t.Fatalf("insert-move = %v", ids)
	}
}

func TestNamespaceIndexClose(t *testing.T) {
	index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Add(Entry{ImageID: 1, Value: EnumStringValue("x")}); err != nil {
			t.Fatalf("Add stage after close = %v, want nil", err)
		}
		if err := batch.Apply(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Add after close = %v, want ErrClosed", err)
		}
	}
	// Writes fail even when the batch is empty.
	{
		batch := index.NewBatch()
		defer batch.Close()
		if err := batch.Apply(); !errors.Is(err, ErrClosed) {
			t.Fatalf("empty Add after close = %v, want ErrClosed", err)
		}
	}
	if err := index.NewBatch().Apply(); !errors.Is(err, ErrClosed) {
		t.Fatalf("empty Apply after close = %v, want ErrClosed", err)
	}
	// Reads report empty results without errors.
	if present, err := index.Contains(1, EnumStringValue("x")); err != nil || present {
		t.Fatalf("Contains after close = %v, %v; want false, nil", present, err)
	}
	if key, ok, err := index.KeyForValue(EnumStringValue("x")); err != nil || ok || key != (NamespaceKey{}) {
		t.Fatalf("KeyForValue after close = %+v, %v, %v; want zero, false, nil", key, ok, err)
	}
	if ids, err := index.ValueIDs(nil, EnumStringValue("x")); err != nil || len(ids) != 0 {
		t.Fatalf("ValueIDs after close = %v, %v; want empty, nil", ids, err)
	}
	if match, err := index.MatchValues(EnumStringValue("x")); err != nil || !match.IsEmpty() {
		t.Fatalf("MatchValues after close = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if match, err := index.MatchValues(); err != nil || !match.IsEmpty() {
		t.Fatalf("empty MatchValues after close = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if count, err := index.CountValues(EnumStringValue("x")); err != nil || count != 0 {
		t.Fatalf("CountValues after close = %d, %v; want 0, nil", count, err)
	}
	if _, ok := index.LookupValue(nil, NamespaceKey{}); ok {
		t.Fatal("LookupValue after close reported presence")
	}
	visited := 0
	index.Iterate(func(NamespaceKey, []byte) bool { visited++; return true })
	index.Prefix(nil, func(NamespaceKey, []byte) bool { visited++; return true })
	if visited != 0 {
		t.Fatalf("Iterate/Prefix after close visited %d entries", visited)
	}
	if err := index.WaitIdle(shortContext(t)); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
}

func TestMatchValuesEmpty(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	defer func() { _ = index.Close() }()
	commit(t, index, func(b *Batch) error {
		return b.Add(Entry{ImageID: 1, Value: EnumStringValue("x")})
	})
	// An empty conjunction matches nothing rather than everything.
	if match, err := index.MatchValues(); err != nil || !match.IsEmpty() {
		t.Fatalf("empty MatchValues = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if count, err := index.CountValues(); err != nil || count != 0 {
		t.Fatalf("empty CountValues = %d, %v; want 0, nil", count, err)
	}
	if got, err := index.FilterValues([]common.ImageID{9}, []common.ImageID{1, 2}); err != nil || !slices.Equal(got, []common.ImageID{9}) {
		t.Fatalf("empty FilterValues = %v, %v; want [9], nil", got, err)
	}
}

func TestDictionaryCompaction(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	defer func() { _ = index.Close() }()
	const values = dictActiveTargetEntries + 100
	adds := make([]Entry, 0, values)
	for n := 0; n < values; n++ {
		adds = append(adds, Entry{
			ImageID: common.ImageID(n + 1),
			Value:   Value{Kind: common.KindBitmapEnum, Bytes: []byte{byte(n), byte(n >> 8)}},
		})
	}
	commit(t, index, func(b *Batch) error { return b.Add(adds...) })
	// The batch exceeds both delta bounds, so it folds straight into a
	// compacted base with no active layer left behind.
	view := index.view.Load()
	if len(view.dict.active) != 0 {
		t.Fatalf("active entries = %d, want 0", len(view.dict.active))
	}
	if len(view.dict.base) != values {
		t.Fatalf("base entries = %d, want %d", len(view.dict.base), values)
	}
	first := Value{Kind: common.KindBitmapEnum, Bytes: []byte{0, 0}}
	if _, ok, err := index.KeyForValue(first); err != nil || !ok {
		t.Fatalf("KeyForValue ok = %v, %v; want true, nil", ok, err)
	}
	if ids, err := index.ValueIDs(nil, first); err != nil || !slices.Equal(ids, []common.ImageID{1}) {
		t.Fatalf("ValueIDs = %v, %v; want [1], nil", ids, err)
	}
	visited := 0
	index.Iterate(func(NamespaceKey, []byte) bool { visited++; return true })
	if visited != values {
		t.Fatalf("Iterate visited %d, want %d", visited, values)
	}
}

func shortContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestReplaceTextRejectsDuplicateImage(t *testing.T) {
	index := mustNamespace(t, textNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}})
	})
	// Two replacements for one image have ambiguous sequential semantics;
	// they must be rejected rather than silently producing a wrong set.
	{
		batch := index.NewBatch()
		defer batch.Close()
		err := batch.ReplaceText(
			TextReplacement{ImageID: 1, Before: []string{"alpha"}, After: []string{"beta"}},
			TextReplacement{ImageID: 1, Before: []string{"beta"}, After: []string{"alpha"}},
		)
		if !errors.Is(err, ErrDuplicateImageID) {
			t.Fatalf("duplicate ReplaceText error = %v, want ErrDuplicateImageID", err)
		}
	}
	match, err := index.MatchText("alpha")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatalf("alpha membership = %v, want [1]", got)
	}
}

// closeWithin runs Close on a goroutine so a leaked reader token surfaces as
// a timeout instead of hanging the test binary.
func closeWithin(t *testing.T, index *Index, what string) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- index.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after %s: %v", what, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Close blocked: %s leaked a reader token", what)
	}
}

func TestIterateCallbackPanicReleasesReader(t *testing.T) {
	index := mustNamespace(t, textNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}})
	})
	func() {
		defer func() { _ = recover() }()
		index.Iterate(func(NamespaceKey, []byte) bool { panic("callback") })
	}()
	closeWithin(t, index, "Iterate callback panic")
}

func TestPrefixCallbackPanicReleasesReader(t *testing.T) {
	index := mustNamespace(t, textNamespace(common.NamespaceTag), NamespaceConfig{})
	commit(t, index, func(b *Batch) error {
		return b.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}})
	})
	func() {
		defer func() { _ = recover() }()
		index.Prefix([]byte("a"), func(NamespaceKey, []byte) bool { panic("callback") })
	}()
	closeWithin(t, index, "Prefix callback panic")
}
