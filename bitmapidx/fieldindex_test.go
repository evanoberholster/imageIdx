package bitmapidx

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
)

const testFocalLength IndexType = 100

func enumField(t IndexType) Field {
	return Field{Type: t, Kind: KindEnum, Storage: StoragePerType,
		Normalizer: CodecSpec{Code: NormalizerIdentity, Version: 1}}
}

func boolField(t IndexType) Field {
	return Field{Type: t, Kind: KindBool, Storage: StoragePerType}
}

func continuousField(t IndexType) Field {
	return Field{Type: t, Kind: KindContinuous, Storage: StoragePerType,
		Quant: Quantizer{Code: QuantIdentity, Version: 1, Scale: 1}}
}

func textField(t IndexType) Field {
	return Field{Type: t, Kind: KindText, Storage: StoragePerType,
		Analyzer: CodecSpec{Code: textanalyzer.CodeLatin, Version: textanalyzer.Version}}
}

func mustField(t *testing.T, field Field, config FieldConfig) *FieldIndex {
	t.Helper()
	index, err := NewFieldIndex(field, config)
	if err != nil {
		t.Fatalf("NewFieldIndex: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func appendIDs(t *testing.T, index *FieldIndex, value Value) []ImageID {
	t.Helper()
	ids, err := index.ValueIDs(nil, value)
	if err != nil {
		t.Fatalf("ValueIDs: %v", err)
	}
	return ids
}

func matchIDs(t *testing.T, index *FieldIndex, values ...Value) []ImageID {
	t.Helper()
	match, err := index.MatchValues(values...)
	if err != nil {
		t.Fatalf("MatchValues: %v", err)
	}
	return match.AppendTo(nil)
}

func TestHashDomainSeparation(t *testing.T) {
	base := hashCanonical(IndexTypeTag, KindEnum, []byte("x"))
	if base != hashCanonical(IndexTypeTag, KindEnum, []byte("x")) {
		t.Fatal("hash is not deterministic")
	}
	if base == hashCanonical(IndexTypeAlbum, KindEnum, []byte("x")) {
		t.Fatal("type is not domain-separated")
	}
	if base == hashCanonical(IndexTypeTag, KindText, []byte("x")) {
		t.Fatal("kind is not domain-separated")
	}
	if base == hashCanonical(IndexTypeTag, KindEnum, []byte("y")) {
		t.Fatal("value is not domain-separated")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(enumField(IndexTypeTag), textField(IndexTypeTag+1)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	data, err := registry.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	restored := NewRegistry()
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if len(restored.Fields()) != 2 {
		t.Fatalf("restored %d fields, want 2", len(restored.Fields()))
	}
	field, ok := restored.Field(IndexTypeTag)
	if !ok || field.Kind != KindEnum {
		t.Fatalf("restored field = %+v, ok=%v", field, ok)
	}

	mismatch := NewRegistry()
	if err := mismatch.Register(enumField(IndexTypeTag)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := mismatch.Register(boolField(IndexTypeTag)); !errors.Is(err, ErrFieldMismatch) {
		t.Fatalf("mismatched register error = %v, want ErrFieldMismatch", err)
	}

	bad := NewRegistry()
	if err := bad.Register(textField(IndexTypeTag + 2)); err != nil {
		t.Fatalf("text register: %v", err)
	}
	badField := textField(IndexTypeTag + 3)
	badField.Analyzer.Code = 99
	if err := bad.Register(badField); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("bad codec error = %v, want ErrInvalidField", err)
	}
}

func TestHashCollisionSlots(t *testing.T) {
	original := hashCanonical
	hashCanonical = func(t IndexType, _ Kind, _ []byte) FieldKey {
		return FieldKey{Type: t, Hash: 0xc0111ded}
	}
	t.Cleanup(func() { hashCanonical = original })

	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(
		Entry{ImageID: 1, Value: StringValue("alpha")},
		Entry{ImageID: 2, Value: StringValue("beta")},
		Entry{ImageID: 3, Value: StringValue("gamma")},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	alphaKey, ok, err := index.KeyForValue(StringValue("alpha"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue(alpha): ok=%v err=%v", ok, err)
	}
	betaKey, ok, err := index.KeyForValue(StringValue("beta"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue(beta): ok=%v err=%v", ok, err)
	}
	gammaKey, ok, err := index.KeyForValue(StringValue("gamma"))
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
		want  []ImageID
	}{
		{StringValue("alpha"), []ImageID{1}},
		{StringValue("beta"), []ImageID{2}},
		{StringValue("gamma"), []ImageID{3}},
	} {
		if got := appendIDs(t, index, check.value); !slices.Equal(got, check.want) {
			t.Fatalf("%s = %v, want %v", check.value.Bytes, got, check.want)
		}
	}
	if present, err := index.Contains(1, StringValue("beta")); err != nil || present {
		t.Fatalf("alpha matched beta: present=%v err=%v", present, err)
	}

	// Removing one colliding value must not disturb the others.
	if err := index.Remove(Entry{ImageID: 2, Value: StringValue("beta")}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := appendIDs(t, index, StringValue("alpha")); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("alpha after beta removal = %v", got)
	}
	if got := appendIDs(t, index, StringValue("gamma")); !slices.Equal(got, []ImageID{3}) {
		t.Fatalf("gamma after beta removal = %v", got)
	}
	if got := appendIDs(t, index, StringValue("beta")); len(got) != 0 {
		t.Fatalf("beta after removal = %v", got)
	}
	if value, ok := index.LookupValue(nil, gammaKey); !ok || string(value) != "gamma" {
		t.Fatalf("LookupValue(gamma) = %q, %v", value, ok)
	}

	// Vacuum drops the dead value and its slot while preserving live keys.
	if err := index.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if _, ok, err := index.KeyForValue(StringValue("beta")); err != nil || ok {
		t.Fatalf("beta after vacuum: ok=%v err=%v", ok, err)
	}
	if got := appendIDs(t, index, StringValue("alpha")); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("alpha after vacuum = %v", got)
	}
	if err := index.Add(Entry{ImageID: 4, Value: StringValue("beta")}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := appendIDs(t, index, StringValue("beta")); !slices.Equal(got, []ImageID{4}) {
		t.Fatalf("beta after re-add = %v", got)
	}
	if got := appendIDs(t, index, StringValue("gamma")); !slices.Equal(got, []ImageID{3}) {
		t.Fatalf("gamma after re-add = %v", got)
	}
	var values []string
	index.Iterate(func(_ FieldKey, value []byte) bool {
		values = append(values, string(value))
		return true
	})
	slices.Sort(values)
	if !slices.Equal(values, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("iterated values = %v", values)
	}
}

func TestFieldIndexEnumLifecycle(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if index.Field().Kind != KindEnum {
		t.Fatalf("kind = %v", index.Field().Kind)
	}
	if err := index.Add(
		Entry{ImageID: 1, Value: StringValue("Paris")},
		Entry{ImageID: 2, Value: StringValue("Paris")},
		Entry{ImageID: 3, Value: StringValue("Rome")},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := matchIDs(t, index, StringValue("Paris")); !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("Paris = %v", got)
	}
	if got := matchIDs(t, index, StringValue("Paris"), StringValue("Rome")); len(got) != 0 {
		t.Fatalf("Paris AND Rome = %v, want empty", got)
	}
	if present, err := index.Contains(1, StringValue("Paris")); err != nil || !present {
		t.Fatalf("Contains(1, Paris) = %v, %v", present, err)
	}
	if present, err := index.Contains(3, StringValue("Paris")); err != nil || present {
		t.Fatalf("Contains(3, Paris) = %v, %v", present, err)
	}

	// Remove wins over Add within one batch.
	if err := index.Apply(FieldMutation{
		Add:    []Entry{{ImageID: 4, Value: StringValue("Paris")}},
		Remove: []Entry{{ImageID: 4, Value: StringValue("Paris")}},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := matchIDs(t, index, StringValue("Paris")); !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("Paris after remove-wins = %v", got)
	}

	if err := index.Remove(Entry{ImageID: 1, Value: StringValue("Paris")}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := matchIDs(t, index, StringValue("Paris")); !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Paris after remove = %v", got)
	}

	count, err := index.CountValues(StringValue("Paris"))
	if err != nil || count != 1 {
		t.Fatalf("CountValues = %d, %v", count, err)
	}
	filtered, err := index.FilterValues(nil, []ImageID{1, 2, 3, 4}, StringValue("Paris"))
	if err != nil || !slices.Equal(filtered, []ImageID{2}) {
		t.Fatalf("FilterValues = %v, %v", filtered, err)
	}
}

func TestFieldIndexBoolExplicitFalse(t *testing.T) {
	index := mustField(t, boolField(IndexTypeFlag), FieldConfig{})
	if err := index.Add(
		Entry{ImageID: 1, Value: BoolValue(false)},
		Entry{ImageID: 2, Value: BoolValue(true)},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if present, _ := index.Contains(1, BoolValue(false)); !present {
		t.Fatal("explicit false is not a member")
	}
	if present, _ := index.Contains(3, BoolValue(false)); present {
		t.Fatal("absent image reported as false member")
	}
	falseIDs := appendIDs(t, index, BoolValue(false))
	if !slices.Equal(falseIDs, []ImageID{1}) {
		t.Fatalf("false IDs = %v", falseIDs)
	}
	trueIDs := appendIDs(t, index, BoolValue(true))
	if !slices.Equal(trueIDs, []ImageID{2}) {
		t.Fatalf("true IDs = %v", trueIDs)
	}
}

func TestFieldIndexContinuousRange(t *testing.T) {
	index := mustField(t, continuousField(testFocalLength), FieldConfig{})
	values := []int64{math.MinInt64, -100, -1, 0, 1, 50, 64, 70, 100, math.MaxInt64}
	for n, value := range values {
		if err := index.Add(Entry{ImageID: ImageID(n + 1), Value: IntValue(value)}); err != nil {
			t.Fatalf("Add %d: %v", value, err)
		}
	}
	check := func(bounds NumericRange, want []int64) {
		t.Helper()
		match, err := index.Range(bounds)
		if err != nil {
			t.Fatalf("Range: %v", err)
		}
		got := match.AppendTo(nil)
		ids := make([]ImageID, 0, len(want))
		for _, value := range want {
			ids = append(ids, ImageID(slices.Index(values, value)+1))
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

	enum := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if _, err := enum.Range(Between(0, 1)); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("Range on enum = %v, want ErrInvalidRange", err)
	}
	if _, err := enum.EstimateRange(Between(0, 1)); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("EstimateRange on enum = %v, want ErrInvalidRange", err)
	}
}

func TestFieldIndexMove(t *testing.T) {
	index := mustField(t, continuousField(testFocalLength), FieldConfig{})
	if err := index.Add(Entry{ImageID: 1, Value: IntValue(24)}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := index.Move(Move{ImageID: 1, From: 24, To: 35, HasFrom: true, HasTo: true}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if ids := appendIDs(t, index, IntValue(24)); len(ids) != 0 {
		t.Fatalf("old value still present: %v", ids)
	}
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []ImageID{1}) {
		t.Fatalf("new value = %v", ids)
	}
	// Same-value move is a no-op, not add/remove cancellation.
	if err := index.Move(Move{ImageID: 1, From: 35, To: 35, HasFrom: true, HasTo: true}); err != nil {
		t.Fatalf("same-value move: %v", err)
	}
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []ImageID{1}) {
		t.Fatalf("same-value move removed membership: %v", ids)
	}
	if err := index.Move(
		Move{ImageID: 1, From: 35, To: 50, HasFrom: true, HasTo: true},
		Move{ImageID: 1, From: 50, To: 85, HasFrom: true, HasTo: true},
	); !errors.Is(err, ErrInvalidMutation) {
		t.Fatalf("duplicate move error = %v, want ErrInvalidMutation", err)
	}
}

func TestFieldIndexTextReplace(t *testing.T) {
	index := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := index.ReplaceText(
		TextReplacement{ImageID: 1, After: []string{"Golden Gate Bridge"}},
		TextReplacement{ImageID: 2, After: []string{"Golden Gate Park"}},
	); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	match, err := index.MatchText("golden gate")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("golden gate = %v", got)
	}
	match, _ = index.MatchText("bridge")
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("bridge = %v", got)
	}
	if err := index.ReplaceText(TextReplacement{ImageID: 1, Before: []string{"Golden Gate Bridge"}, After: []string{"Golden Gate Park"}}); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	match, _ = index.MatchText("bridge")
	if got := match.AppendTo(nil); len(got) != 0 {
		t.Fatalf("bridge after replace = %v", got)
	}
	// Token retained in both old and new text must not be removed.
	match, _ = index.MatchText("golden")
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("golden after overlapping replace = %v", got)
	}
	// Zero-token queries match nothing.
	match, _ = index.MatchText("!!!")
	if got := match.AppendTo(nil); len(got) != 0 {
		t.Fatalf("empty token query = %v", got)
	}
	if err := index.Add(Entry{ImageID: 3, Value: Value{Kind: KindText, Bytes: []byte("x")}}); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("text Add error = %v, want ErrUnsupportedOperation", err)
	}
}

func TestFieldIndexDeltaRotationAndCompaction(t *testing.T) {
	config := FieldConfig{ActiveTarget: 3, HardDeltaLimit: 8, MaxDeltaValues: 2}
	index := mustField(t, enumField(IndexTypeTag), config)
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("a")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Cross ActiveTarget to rotate into Frozen and schedule compaction.
	if err := index.Add(
		Entry{ImageID: 2, Value: StringValue("a")},
		Entry{ImageID: 3, Value: StringValue("a")},
		Entry{ImageID: 4, Value: StringValue("a")},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := index.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := matchIDs(t, index, StringValue("a")); !slices.Equal(got, []ImageID{1, 2, 3, 4}) {
		t.Fatalf("after rotation = %v", got)
	}
	// A batch above HardDeltaLimit folds the base directly.
	large := make([]Entry, 0, 12)
	for id := ImageID(10); id < 22; id++ {
		large = append(large, Entry{ImageID: id, Value: StringValue("b")})
	}
	if err := index.Add(large...); err != nil {
		t.Fatalf("large add: %v", err)
	}
	if got := matchIDs(t, index, StringValue("b")); len(got) != 12 {
		t.Fatalf("large add = %v", got)
	}
	if got := matchIDs(t, index, StringValue("a")); !slices.Equal(got, []ImageID{1, 2, 3, 4}) {
		t.Fatalf("base preserved = %v", got)
	}

	// Delta beyond MaxDeltaValues folds too.
	for value := 0; value < 4; value++ {
		if err := index.Add(Entry{ImageID: ImageID(100 + value), Value: StringValue(string(rune('p' + value)))}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if got := matchIDs(t, index, StringValue("p")); !slices.Equal(got, []ImageID{100}) {
		t.Fatalf("folded value = %v", got)
	}
}

func TestFieldIndexVacuumDropsDeadValues(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(
		Entry{ImageID: 1, Value: StringValue("dead")},
		Entry{ImageID: 2, Value: StringValue("live")},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := index.Remove(Entry{ImageID: 1, Value: StringValue("dead")}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, err := index.KeyForValue(StringValue("dead")); err != nil || !ok {
		t.Fatalf("dead before vacuum: ok=%v err=%v", ok, err)
	}
	if err := index.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if _, ok, err := index.KeyForValue(StringValue("dead")); err != nil || ok {
		t.Fatalf("dead after vacuum: ok=%v err=%v", ok, err)
	}
	if got := matchIDs(t, index, StringValue("live")); !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("live after vacuum = %v", got)
	}
	var visited []string
	index.Iterate(func(_ FieldKey, value []byte) bool {
		visited = append(visited, string(value))
		return true
	})
	if !slices.Equal(visited, []string{"live"}) {
		t.Fatalf("iterated values = %v", visited)
	}
}

func TestFieldIndexPrefixAndLookup(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(
		Entry{ImageID: 1, Value: StringValue("Trekking")},
		Entry{ImageID: 2, Value: StringValue("Travel")},
		Entry{ImageID: 3, Value: StringValue("Family")},
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	key, ok, err := index.KeyForValue(StringValue("Travel"))
	if err != nil || !ok {
		t.Fatalf("KeyForValue: ok=%v err=%v", ok, err)
	}
	if value, ok := index.LookupValue(nil, key); !ok || string(value) != "Travel" {
		t.Fatalf("LookupValue = %q, %v", value, ok)
	}
	var prefixed []string
	index.Prefix([]byte("Tr"), func(_ FieldKey, value []byte) bool {
		prefixed = append(prefixed, string(value))
		return true
	})
	if !slices.Equal(prefixed, []string{"Travel", "Trekking"}) {
		t.Fatalf("prefix = %v", prefixed)
	}
	// Removal-only batches never create dictionary entries.
	if err := index.Remove(Entry{ImageID: 9, Value: StringValue("Ghost")}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, _ := index.KeyForValue(StringValue("Ghost")); ok {
		t.Fatal("removal-only value was created")
	}
}

func TestFieldIndexValidation(t *testing.T) {
	if _, err := NewFieldIndex(Field{Type: 0, Kind: KindEnum, Storage: StoragePerType}, FieldConfig{}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("zero type error = %v", err)
	}
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(Entry{ImageID: 0, Value: StringValue("x")}); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("zero id error = %v", err)
	}
	if err := index.Add(Entry{ImageID: 1, Value: BoolValue(true)}); !errors.Is(err, ErrValueKind) {
		t.Fatalf("wrong kind error = %v", err)
	}
}

func TestFieldIndexConcurrentReadersWriters(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{ActiveTarget: 64, HardDeltaLimit: 128})
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
				_, _ = index.MatchValues(StringValue("a"))
				_ = index.ContainsKey(1, hashCanonical(IndexTypeTag, KindEnum, []byte("a")))
				_, _, _ = index.KeyForValue(StringValue("a"))
			}
		}()
	}
	for batch := 0; batch < 200; batch++ {
		id := ImageID(batch + 1)
		if err := index.Add(Entry{ImageID: id, Value: StringValue("a")}); err != nil {
			t.Errorf("Add: %v", err)
			break
		}
		if batch%3 == 0 {
			if err := index.Remove(Entry{ImageID: id, Value: StringValue("a")}); err != nil {
				t.Errorf("Remove: %v", err)
				break
			}
		}
	}
	close(stop)
	wait.Wait()
}

func TestFieldIndexTextAnalyzerPipeline(t *testing.T) {
	index := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := index.ReplaceText(TextReplacement{ImageID: 1, After: []string{"Café au lait!"}}); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	match, err := index.MatchText("CAFE lait")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1}) {
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

func TestFieldIndexMoveToEmpty(t *testing.T) {
	index := mustField(t, continuousField(testFocalLength), FieldConfig{})
	if err := index.Add(Entry{ImageID: 1, Value: IntValue(24)}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := index.Move(Move{ImageID: 1, HasFrom: true, From: 24, To: 0, HasTo: false}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if ids := appendIDs(t, index, IntValue(24)); len(ids) != 0 {
		t.Fatalf("value after remove-move = %v", ids)
	}
	if err := index.Move(Move{ImageID: 2, HasTo: true, To: 35}); err != nil {
		t.Fatalf("Move insert: %v", err)
	}
	if ids := appendIDs(t, index, IntValue(35)); !slices.Equal(ids, []ImageID{2}) {
		t.Fatalf("insert-move = %v", ids)
	}
}

func TestFieldIndexClose(t *testing.T) {
	index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
	if err != nil {
		t.Fatalf("NewFieldIndex: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("x")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Add after close = %v, want ErrClosed", err)
	}
	// Writes fail even when the batch is empty.
	if err := index.Add(); !errors.Is(err, ErrClosed) {
		t.Fatalf("empty Add after close = %v, want ErrClosed", err)
	}
	if err := index.Apply(FieldMutation{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("empty Apply after close = %v, want ErrClosed", err)
	}
	// Reads report empty results without errors.
	if present, err := index.Contains(1, StringValue("x")); err != nil || present {
		t.Fatalf("Contains after close = %v, %v; want false, nil", present, err)
	}
	if key, ok, err := index.KeyForValue(StringValue("x")); err != nil || ok || key != (FieldKey{}) {
		t.Fatalf("KeyForValue after close = %+v, %v, %v; want zero, false, nil", key, ok, err)
	}
	if ids, err := index.ValueIDs(nil, StringValue("x")); err != nil || len(ids) != 0 {
		t.Fatalf("ValueIDs after close = %v, %v; want empty, nil", ids, err)
	}
	if match, err := index.MatchValues(StringValue("x")); err != nil || !match.IsEmpty() {
		t.Fatalf("MatchValues after close = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if match, err := index.MatchValues(); err != nil || !match.IsEmpty() {
		t.Fatalf("empty MatchValues after close = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if count, err := index.CountValues(StringValue("x")); err != nil || count != 0 {
		t.Fatalf("CountValues after close = %d, %v; want 0, nil", count, err)
	}
	if ids := index.AppendTo(nil); len(ids) != 0 {
		t.Fatalf("AppendTo after close = %v; want empty", ids)
	}
	if _, ok := index.LookupValue(nil, FieldKey{}); ok {
		t.Fatal("LookupValue after close reported presence")
	}
	visited := 0
	index.Iterate(func(FieldKey, []byte) bool { visited++; return true })
	index.Prefix(nil, func(FieldKey, []byte) bool { visited++; return true })
	if visited != 0 {
		t.Fatalf("Iterate/Prefix after close visited %d entries", visited)
	}
	if err := index.WaitIdle(shortContext(t)); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
}

func TestMatchValuesEmpty(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	defer func() { _ = index.Close() }()
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("x")}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// An empty conjunction matches nothing rather than everything.
	if match, err := index.MatchValues(); err != nil || !match.IsEmpty() {
		t.Fatalf("empty MatchValues = %v, %v; want empty, nil", match.AppendTo(nil), err)
	}
	if count, err := index.CountValues(); err != nil || count != 0 {
		t.Fatalf("empty CountValues = %d, %v; want 0, nil", count, err)
	}
	if got, err := index.FilterValues([]ImageID{9}, []ImageID{1, 2}); err != nil || !slices.Equal(got, []ImageID{9}) {
		t.Fatalf("empty FilterValues = %v, %v; want [9], nil", got, err)
	}
}

func TestDictionaryCompaction(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	defer func() { _ = index.Close() }()
	const values = dictActiveTargetEntries + 100
	adds := make([]Entry, 0, values)
	for n := 0; n < values; n++ {
		adds = append(adds, Entry{
			ImageID: ImageID(n + 1),
			Value:   Value{Kind: KindEnum, Bytes: []byte{byte(n), byte(n >> 8)}},
		})
	}
	if err := index.Add(adds...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The batch exceeds both delta bounds, so it folds straight into a
	// compacted base with no active layer left behind.
	view := index.view.Load()
	if len(view.dict.active) != 0 {
		t.Fatalf("active entries = %d, want 0", len(view.dict.active))
	}
	if len(view.dict.base) != values {
		t.Fatalf("base entries = %d, want %d", len(view.dict.base), values)
	}
	first := Value{Kind: KindEnum, Bytes: []byte{0, 0}}
	if _, ok, err := index.KeyForValue(first); err != nil || !ok {
		t.Fatalf("KeyForValue ok = %v, %v; want true, nil", ok, err)
	}
	if ids, err := index.ValueIDs(nil, first); err != nil || !slices.Equal(ids, []ImageID{1}) {
		t.Fatalf("ValueIDs = %v, %v; want [1], nil", ids, err)
	}
	visited := 0
	index.Iterate(func(FieldKey, []byte) bool { visited++; return true })
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
	index := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := index.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}}); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	// Two replacements for one image have ambiguous sequential semantics;
	// they must be rejected rather than silently producing a wrong set.
	err := index.ReplaceText(
		TextReplacement{ImageID: 1, Before: []string{"alpha"}, After: []string{"beta"}},
		TextReplacement{ImageID: 1, Before: []string{"beta"}, After: []string{"alpha"}},
	)
	if !errors.Is(err, ErrDuplicateImageID) {
		t.Fatalf("duplicate ReplaceText error = %v, want ErrDuplicateImageID", err)
	}
	match, err := index.MatchText("alpha")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("alpha membership = %v, want [1]", got)
	}
}

// closeWithin runs Close on a goroutine so a leaked reader token surfaces as
// a timeout instead of hanging the test binary.
func closeWithin(t *testing.T, index *FieldIndex, what string) {
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
	index := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := index.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}}); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		index.Iterate(func(FieldKey, []byte) bool { panic("callback") })
	}()
	closeWithin(t, index, "Iterate callback panic")
}

func TestPrefixCallbackPanicReleasesReader(t *testing.T) {
	index := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := index.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha"}}); err != nil {
		t.Fatalf("ReplaceText: %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		index.Prefix([]byte("a"), func(FieldKey, []byte) bool { panic("callback") })
	}()
	closeWithin(t, index, "Prefix callback panic")
}
