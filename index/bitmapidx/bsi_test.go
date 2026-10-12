package bitmapidx

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

const (
	testBSI16Namespace common.Namespace = 200
	testBSI32Namespace common.Namespace = 201
)

func bsiNamespace(t common.Namespace, width uint8) NamespaceSpec {
	return NamespaceSpec{Namespace: t, Kind: common.KindBitmapBSI, BSIWidth: width}
}

func mustBSI(t *testing.T, namespace NamespaceSpec) *Index {
	t.Helper()
	index, err := NewMemory(namespace, NamespaceConfig{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

// refBSI is the simple map-based reference implementation the bit-sliced index
// is checked against.
type refBSI struct {
	values map[common.ValueID]uint64
}

func newRefBSI() *refBSI { return &refBSI{values: make(map[common.ValueID]uint64)} }

func (r *refBSI) set(id common.ValueID, value uint64) { r.values[id] = value }
func (r *refBSI) del(id common.ValueID)               { delete(r.values, id) }
func (r *refBSI) get(id common.ValueID) (uint64, bool) {
	v, ok := r.values[id]
	return v, ok
}

func (r *refBSI) equal(value uint64) []common.ValueID {
	var out []common.ValueID
	for id, v := range r.values {
		if v == value {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (r *refBSI) less(value uint64) []common.ValueID {
	var out []common.ValueID
	for id, v := range r.values {
		if v < value {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (r *refBSI) inRange(lo, hi uint64) []common.ValueID {
	var out []common.ValueID
	for id, v := range r.values {
		if v >= lo && v <= hi {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func bitmapIDs(t *testing.T, bit interface{ AppendTo([]uint64) []uint64 }) []common.ValueID {
	t.Helper()
	return bit.AppendTo(nil)
}

func assertIDs(t *testing.T, got, want []common.ValueID) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestBSINamespaceValidation(t *testing.T) {
	if _, err := NewMemory(NamespaceSpec{Namespace: 1, Kind: common.KindBitmapBSI, BSIWidth: 8}, NamespaceConfig{}); !errors.Is(err, ErrInvalidBSIWidth) {
		t.Fatalf("width 8 err = %v, want ErrInvalidBSIWidth", err)
	}
	if _, err := NewMemory(NamespaceSpec{Namespace: 1, Kind: common.KindBitmapBSI, BSIWidth: 0}, NamespaceConfig{}); !errors.Is(err, ErrInvalidBSIWidth) {
		t.Fatalf("width 0 err = %v, want ErrInvalidBSIWidth", err)
	}
	if _, err := NewMemory(NamespaceSpec{Namespace: 1, Kind: common.KindBitmapEnum, Normalizer: CodecSpec{Code: NormalizerIdentity, Version: 1}, BSIWidth: 16}, NamespaceConfig{}); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("enum with width err = %v, want ErrInvalidNamespace", err)
	}
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		if _, err := NewMemory(bsiNamespace(1, width), NamespaceConfig{}); err != nil {
			t.Fatalf("width %d rejected: %v", width, err)
		}
	}
}

func TestBSIPersistWidthRoundTrip(t *testing.T) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		spec := bsiNamespace(7, width)
		def, err := spec.NamespaceDef("bsi")
		if err != nil {
			t.Fatal(err)
		}
		got, err := NamespaceSpecFromSchemaDef(def)
		if err != nil {
			t.Fatal(err)
		}
		if got.BSIWidth != width || got.Kind != common.KindBitmapBSI || got.Namespace != 7 {
			t.Fatalf("round trip = %+v, want width %d", got, width)
		}
	}
}

func TestBSISetGetEqualRange16(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	sets := map[common.ValueID]uint64{
		1: 0, 2: 1, 3: 255, 4: 256, 5: 65535, 6: 32768, 7: 100, 8: 12345,
	}
	ref := newRefBSI()
	for id, value := range sets {
		if err := index.Set(id, value); err != nil {
			t.Fatalf("Set(%d,%d): %v", id, value, err)
		}
		ref.set(id, value)
	}
	for id, want := range sets {
		got, ok, err := index.Get(id)
		if err != nil || !ok || got != want {
			t.Fatalf("Get(%d) = %d,%v,%v want %d", id, got, ok, err, want)
		}
	}
	for _, value := range []uint64{0, 1, 100, 255, 256, 12345, 32768, 65535, 42} {
		bit, err := index.Equal(value)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, bitmapIDs(t, bit), ref.equal(value))
	}
	ranges := [][2]uint64{{0, 0}, {0, 255}, {256, 65535}, {100, 12345}, {101, 12344}, {65535, 65535}}
	for _, r := range ranges {
		bit, err := index.BSIRange(r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, bitmapIDs(t, bit), ref.inRange(r[0], r[1]))
	}
}

func TestBSIValueZeroAndMax32(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI32Namespace, BSIWidth32))
	if err := index.Set(1, 0); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := index.Get(1); err != nil || !ok || v != 0 {
		t.Fatalf("zero value: %d,%v,%v", v, ok, err)
	}
	if err := index.Set(2, 1<<32-1); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := index.Get(2); !ok || v != 1<<32-1 {
		t.Fatalf("max uint32 = %d,%v", v, ok)
	}
	// Width overflow is rejected, never truncated.
	if err := index.Set(3, 1<<32); !errors.Is(err, ErrBSIValueRange) {
		t.Fatalf("Set overflow err = %v, want ErrBSIValueRange", err)
	}
	if _, err := index.Equal(1 << 32); !errors.Is(err, ErrBSIValueRange) {
		t.Fatalf("Equal overflow err = %v, want ErrBSIValueRange", err)
	}
	// Zero is a real value distinct from absent.
	if err := index.Delete(1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := index.Get(1); ok {
		t.Fatal("deleted zero value still present")
	}
}

func TestBSIInvalidID(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	if err := index.Set(0, 5); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("Set(0) err = %v", err)
	}
	if err := index.Delete(0); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("Delete(0) err = %v", err)
	}
	if _, _, err := index.Get(0); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("Get(0) err = %v", err)
	}
}

func TestBSIUpdateOnlyTouchesChangedBits(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI32Namespace, BSIWidth32))
	// 0b1010 -> 0b0010: only bit 3 clears.
	if err := index.Set(1, 0b1010); err != nil {
		t.Fatal(err)
	}
	if err := index.Set(1, 0b0010); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := index.Get(1); !ok || v != 0b0010 {
		t.Fatalf("value = %d,%v", v, ok)
	}
	bit, _ := index.Equal(0b0010)
	assertIDs(t, bitmapIDs(t, bit), []common.ValueID{1})
	notSet, _ := index.Equal(0b1010)
	if !notSet.IsEmpty() {
		t.Fatalf("old value still matches: %v", bitmapIDs(t, notSet))
	}
}

func TestBSIComparisons(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	ref := newRefBSI()
	values := []uint64{0, 1, 2, 7, 8, 100, 255, 256, 65535}
	for i, v := range values {
		id := common.ValueID(i + 1)
		if err := index.Set(id, v); err != nil {
			t.Fatal(err)
		}
		ref.set(id, v)
	}
	for _, probe := range []uint64{0, 1, 5, 8, 99, 256, 65535} {
		got, err := index.Less(probe)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, bitmapIDs(t, got), ref.less(probe))

		got, _ = index.LessOrEqual(probe)
		assertIDs(t, bitmapIDs(t, got), ref.inRange(0, probe))

		got, _ = index.Greater(probe)
		want := ref.inRange(probe+1, 65535)
		if probe == 65535 {
			want = nil
		}
		assertIDs(t, bitmapIDs(t, got), want)

		got, _ = index.GreaterOrEqual(probe)
		assertIDs(t, bitmapIDs(t, got), ref.inRange(probe, 65535))

		got, _ = index.NotEqual(probe)
		var wantNE []common.ValueID
		for id, v := range ref.values {
			if v != probe {
				wantNE = append(wantNE, id)
			}
		}
		slices.Sort(wantNE)
		assertIDs(t, bitmapIDs(t, got), wantNE)
	}
}

func TestBSIRangeInclusiveExclusiveAndEmpty(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	ref := newRefBSI()
	for i := 0; i < 32; i++ {
		id := common.ValueID(i + 1)
		value := uint64(i * 100)
		_ = index.Set(id, value)
		ref.set(id, value)
	}
	// exclusive lower, exclusive upper: (100, 400) -> 200,300
	bit, err := index.BSIRangeQuery(ValueRange{Lo: 100, Hi: 400, HasLo: true, HasHi: true})
	if err != nil {
		t.Fatal(err)
	}
	want := ref.inRange(200, 300)
	assertIDs(t, bitmapIDs(t, bit), want)

	// empty: inverted bounds and equal bounds exclusive.
	bit, _ = index.BSIRangeQuery(ValueRange{Lo: 500, Hi: 100, HasLo: true, HasHi: true})
	if !bit.IsEmpty() {
		t.Fatalf("inverted range not empty: %v", bitmapIDs(t, bit))
	}
	bit, _ = index.BSIRangeQuery(ValueRange{Lo: 100, Hi: 100, HasLo: true, HasHi: true})
	if !bit.IsEmpty() {
		t.Fatalf("exclusive equal range not empty: %v", bitmapIDs(t, bit))
	}
	// count matches.
	count, err := index.BSICountRange(ValuesBetween(200, 300))
	if err != nil || count != uint64(len(want)) {
		t.Fatalf("count = %d, want %d (%v)", count, len(want), err)
	}
}

func TestBSIRangeRandomizedInclusiveExclusive(t *testing.T) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		index := mustBSI(t, bsiNamespace(testBSI16Namespace, width))
		ref := newRefBSI()
		rng := rand.New(rand.NewSource(int64(width) + 7))
		max := bsiMaxValue(width)
		for i := 0; i < 1500; i++ {
			id := common.ValueID(rng.Intn(300) + 1)
			if rng.Intn(4) == 0 {
				if err := index.Delete(id); err != nil {
					t.Fatal(err)
				}
				ref.del(id)
			} else {
				value := uint64(rng.Int63n(int64(max) + 1))
				if err := index.Set(id, value); err != nil {
					t.Fatal(err)
				}
				ref.set(id, value)
			}
		}
		for trial := 0; trial < 400; trial++ {
			lo := uint64(rng.Int63n(int64(max) + 1))
			hi := uint64(rng.Int63n(int64(max) + 1))
			if lo > hi {
				lo, hi = hi, lo
			}
			r := ValueRange{Lo: lo, Hi: hi, HasLo: true, HasHi: true, LoInclusive: rng.Intn(2) == 0, HiInclusive: rng.Intn(2) == 0}
			bit, err := index.BSIRangeQuery(r)
			if err != nil {
				t.Fatal(err)
			}
			got := bit.AppendTo(nil)
			var want []common.ValueID
			for id, value := range ref.values {
				if r.contains(value) {
					want = append(want, id)
				}
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("width %d range %+v: got %v want %v", width, r, got, want)
			}
			count, err := index.BSICountRange(r)
			if err != nil || count != uint64(len(want)) {
				t.Fatalf("width %d range %+v: count %d (%v) want %d", width, r, count, err, len(want))
			}
		}
	}
}

// TestBSIRangeBoundaries is a table-driven boundary check for both widths:
// stored zero vs absent, the width extremes, inclusive/exclusive single points,
// open-ended and full-domain ranges, and empty/inverted ranges. It cross-checks
// BSIRangeQuery, BSICountRange, and BSIFilterRange against a map reference.
func TestBSIRangeBoundaries(t *testing.T) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		max := bsiMaxValue(width)
		index := mustBSI(t, bsiNamespace(testBSI16Namespace, width))
		ref := newRefBSI()
		for id, value := range map[common.ValueID]uint64{
			1: 0, 2: max, 3: max - 1, 4: 1, 5: max / 2, 6: 2, 7: max / 2,
		} {
			if err := index.Set(id, value); err != nil {
				t.Fatal(err)
			}
			ref.set(id, value)
		}
		// Candidates include present and absent IDs and deliberate duplicates.
		candidates := []common.ValueID{1, 2, 3, 8, 5, 99, 4, 5, 9, 6, 7}
		cases := []struct {
			name string
			r    ValueRange
		}{
			{"eq[0,0]", ValueRange{Lo: 0, Hi: 0, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}},
			{"(0,1)", ValueRange{Lo: 0, Hi: 1, HasLo: true, HasHi: true}},
			{"[0,max]", ValueRange{Lo: 0, Hi: max, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}},
			{"[max,max]", ValueRange{Lo: max, Hi: max, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}},
			{"[0,max)", ValueRange{Lo: 0, Hi: max, HasLo: true, HasHi: true, LoInclusive: true}},
			{"(0,max]", ValueRange{Lo: 0, Hi: max, HasLo: true, HasHi: true, HiInclusive: true}},
			{"[0,0)", ValueRange{Lo: 0, Hi: 0, HasLo: true, HasHi: true, LoInclusive: true}},
			{"(max,max]", ValueRange{Lo: max, Hi: max, HasLo: true, HasHi: true, HiInclusive: true}},
			{"atleast0", ValuesAtLeast(0)},
			{"atleast1", ValuesAtLeast(1)},
			{"atmostMax", ValuesAtMost(max)},
			{"atmostMaxMinus1", ValuesAtMost(max - 1)},
			{"[max/2,max/2]", ValuesBetween(max/2, max/2)},
			{"inverted", ValueRange{Lo: max, Hi: 0, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}},
			{"emptyEqual", ValueRange{Lo: 5, Hi: 5, HasLo: true, HasHi: true}},
			{"unbounded", ValueRange{}},
		}
		for _, tc := range cases {
			bit, err := index.BSIRangeQuery(tc.r)
			if err != nil {
				t.Fatalf("width %d %s: BSIRangeQuery: %v", width, tc.name, err)
			}
			got := bit.AppendTo(nil)
			var want []common.ValueID
			for id, value := range ref.values {
				if tc.r.contains(value) {
					want = append(want, id)
				}
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("width %d %s: range got %v want %v", width, tc.name, got, want)
			}
			count, err := index.BSICountRange(tc.r)
			if err != nil || count != uint64(len(want)) {
				t.Fatalf("width %d %s: count %d (%v) want %d", width, tc.name, count, err, len(want))
			}
			filtered, err := index.BSIFilterRange(nil, candidates, tc.r)
			if err != nil {
				t.Fatalf("width %d %s: BSIFilterRange: %v", width, tc.name, err)
			}
			var wantFiltered []common.ValueID
			for _, id := range candidates {
				if value, ok := ref.get(id); ok && tc.r.contains(value) {
					wantFiltered = append(wantFiltered, id)
				}
			}
			if !slices.Equal(filtered, wantFiltered) {
				t.Fatalf("width %d %s: filter got %v want %v", width, tc.name, filtered, wantFiltered)
			}
		}
	}
}

// TestBSIConcurrentQueriesWithMaintenance races readers (which retain results)
// against a writer plus checkpoint and compaction, verifying pooled scratch and
// returned bitmaps stay valid. Intended for -race.
func TestBSIConcurrentQueriesWithMaintenance(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, BSIWidth16)}, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, ok := store.Lookup(testBSI16Namespace)
	if !ok {
		t.Fatal("namespace missing")
	}
	seed := index.NewBatch()
	for i := 1; i <= 2000; i++ {
		if err := seed.BSIMove(BSIMove{ID: common.ValueID(i), To: uint32(i % 256), HasTo: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		t.Fatal(err)
	}

	// Readers run until stop; the writer and maintenance finish first so the
	// checkpoints/compactions overlap the write stream without running forever.
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	var readers, work sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var held []roar.Bitmap
			for {
				select {
				case <-stop:
					// Retained results must survive scratch reuse, checkpoint,
					// compaction, and mmap remapping.
					for _, b := range held {
						_ = b.AppendTo(nil)
					}
					return
				default:
				}
				if b, err := index.BSIRange(10, 200); err == nil && len(held) < 8 {
					held = append(held, b)
				}
				if _, err := index.Equal(50); err != nil {
					t.Error(err)
					return
				}
				if _, err := index.BSICountRange(ValuesBetween(0, 128)); err != nil {
					t.Error(err)
					return
				}
				if _, err := index.BSIFilterRange(nil, []common.ValueID{1, 2, 3, 2000, 7}, ValuesBetween(0, 255)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	work.Add(1)
	go func() {
		defer work.Done()
		for i := 0; i < 3000; i++ {
			if err := index.Set(common.ValueID(i%2000+1), uint64(i%256)); err != nil {
				t.Error(err)
				return
			}
		}
		close(writerDone)
	}()
	work.Add(1)
	go func() {
		defer work.Done()
		for cycle := 0; cycle < 8; cycle++ {
			select {
			case <-writerDone:
				return
			default:
			}
			if err := store.Checkpoint(); err != nil {
				t.Error(err)
				return
			}
			if err := index.Compact(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	work.Wait()
	close(stop)
	readers.Wait()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBSIFilterRangePreservesOrderAndDuplicates(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	for i := 1; i <= 20; i++ {
		_ = index.Set(common.ValueID(i), uint64(i*10))
	}
	candidates := []common.ValueID{5, 3, 3, 17, 100, 1, 12}
	got, err := index.BSIFilterRange(nil, candidates, ValuesBetween(30, 120))
	if err != nil {
		t.Fatal(err)
	}
	want := []common.ValueID{5, 3, 3, 12}
	assertIDs(t, got, want)

	// In-place destination reuse is safe.
	got2, err := index.BSIFilterRange(candidates[:0], candidates, ValuesBetween(30, 120))
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, got2, want)
}

func TestBSIStaleAndConflictingBatches(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	if err := index.Set(1, 10); err != nil {
		t.Fatal(err)
	}
	// Update with a stale From fails.
	batch := index.NewBatch()
	if err := batch.BSIMove(BSIMove{ID: 1, From: 99, To: 20, HasFrom: true, HasTo: true}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrBSIStale) {
		t.Fatalf("stale update err = %v, want ErrBSIStale", err)
	}
	if v, _, _ := index.Get(1); v != 10 {
		t.Fatalf("value changed despite stale From: %d", v)
	}
	// Insert on an existing ID fails.
	batch.Reset()
	if err := batch.BSIMove(BSIMove{ID: 1, To: 20, HasTo: true}); err != nil {
		t.Fatal(err)
	}
	_ = batch.Apply()
	if err := index.Barrier(); !errors.Is(err, ErrBSIExists) {
		t.Fatalf("insert-existing err = %v, want ErrBSIExists", err)
	}
	// Duplicate IDs in one batch are rejected at staging.
	batch.Reset()
	if err := batch.BSIMove(BSIMove{ID: 2, To: 1, HasTo: true}, BSIMove{ID: 2, To: 2, HasTo: true}); !errors.Is(err, ErrBSIDuplicate) {
		t.Fatalf("duplicate err = %v, want ErrBSIDuplicate", err)
	}
	// Correct conditional update succeeds and is atomic.
	batch.Reset()
	if err := batch.BSIMove(BSIMove{ID: 1, From: 10, To: 20, HasFrom: true, HasTo: true}); err != nil {
		t.Fatal(err)
	}
	_ = batch.Apply()
	if err := index.Barrier(); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := index.Get(1); v != 20 {
		t.Fatalf("update value = %d, want 20", v)
	}
}

func TestBSIBatchAtomicMultiBit(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI32Namespace, BSIWidth32))
	batch := index.NewBatch()
	for i := 1; i <= 64; i++ {
		if err := batch.BSIMove(BSIMove{ID: common.ValueID(i), To: uint32(i * 1000), HasTo: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		t.Fatal(err)
	}
	if index.Cardinality() != 64 {
		t.Fatalf("cardinality = %d, want 64", index.Cardinality())
	}
	// An atomic batch that fails on one stale op applies nothing.
	batch.Reset()
	_ = batch.BSIMove(
		BSIMove{ID: 1, From: 1000, To: 2000, HasFrom: true, HasTo: true},
		BSIMove{ID: 2, From: 999, To: 2000, HasFrom: true, HasTo: true}, // stale
	)
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrBSIStale) {
		t.Fatalf("batch err = %v, want ErrBSIStale", err)
	}
	if v, _, _ := index.Get(1); v != 1000 {
		t.Fatalf("partial batch applied: id1 = %d", v)
	}
	if v, _, _ := index.Get(2); v != 2000 {
		t.Fatalf("partial batch applied: id2 = %d", v)
	}
}

func TestBSIRandomizedAgainstReference(t *testing.T) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		index := mustBSI(t, bsiNamespace(testBSI16Namespace, width))
		ref := newRefBSI()
		rng := rand.New(rand.NewSource(int64(width)))
		max := bsiMaxValue(width)
		for op := 0; op < 2000; op++ {
			id := common.ValueID(rng.Intn(512) + 1)
			switch rng.Intn(3) {
			case 0, 1:
				value := uint64(rng.Int63n(int64(max))) + 1
				if value > max {
					value = max
				}
				if err := index.Set(id, value); err != nil {
					t.Fatal(err)
				}
				ref.set(id, value)
			case 2:
				if err := index.Delete(id); err != nil {
					t.Fatal(err)
				}
				ref.del(id)
			}
		}
		for probe := uint64(0); probe < 64; probe++ {
			value := rng.Int63n(int64(max))
			got, _ := index.Equal(uint64(value))
			assertIDs(t, bitmapIDs(t, got), ref.equal(uint64(value)))
			got, _ = index.Less(uint64(value))
			assertIDs(t, bitmapIDs(t, got), ref.less(uint64(value)))
		}
		for _, r := range [][2]uint64{{0, max}, {0, max / 2}, {max / 4, 3 * max / 4}, {max, max}} {
			bit, _ := index.BSIRange(r[0], r[1])
			assertIDs(t, bitmapIDs(t, bit), ref.inRange(r[0], r[1]))
		}
		// Get agrees for every id seen.
		for id, want := range ref.values {
			got, ok, err := index.Get(id)
			if err != nil || !ok || got != want {
				t.Fatalf("width %d Get(%d) = %d,%v,%v want %d", width, id, got, ok, err, want)
			}
		}
	}
}

func TestBSIConcurrentReadersWriters(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI32Namespace, BSIWidth32))
	for i := 1; i <= 256; i++ {
		_ = index.Set(common.ValueID(i), uint64(i))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = index.Equal(100)
					_, _ = index.BSIRange(50, 200)
					_, _, _ = index.Get(100)
				}
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		id := common.ValueID(i%256 + 1)
		if i%5 == 0 {
			_ = index.Delete(id)
		} else {
			_ = index.Set(id, uint64(i))
		}
	}
	close(stop)
	wg.Wait()
}

func TestBSICompaction(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	ref := newRefBSI()
	rng := rand.New(rand.NewSource(9))
	for op := 0; op < 1000; op++ {
		id := common.ValueID(rng.Intn(200) + 1)
		if rng.Intn(4) == 0 {
			_ = index.Delete(id)
			ref.del(id)
		} else {
			value := uint64(rng.Intn(65536))
			_ = index.Set(id, value)
			ref.set(id, value)
		}
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	got, _ := index.Equal(0)
	assertIDs(t, bitmapIDs(t, got), ref.equal(0))
	if index.Cardinality() != uint64(len(ref.values)) {
		t.Fatalf("cardinality after compact = %d, want %d", index.Cardinality(), len(ref.values))
	}
}

func TestBSIPersistentCheckpointReopen(t *testing.T) {
	dir := t.TempDir()
	spec := bsiNamespace(testBSI32Namespace, BSIWidth32)
	store, err := NewStore(dir, []NamespaceSpec{spec}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, ok := store.Lookup(testBSI32Namespace)
	if !ok {
		t.Fatal("namespace missing")
	}
	ref := newRefBSI()
	for i := 1; i <= 300; i++ {
		value := uint64(i * 7)
		if err := index.Set(common.ValueID(i), value); err != nil {
			t.Fatal(err)
		}
		ref.set(common.ValueID(i), value)
	}
	for i := 1; i <= 300; i += 3 {
		_ = index.Delete(common.ValueID(i))
		ref.del(common.ValueID(i))
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.Lookup(testBSI32Namespace)
	if !ok {
		t.Fatal("namespace missing after reopen")
	}
	if got.Spec().BSIWidth != BSIWidth32 {
		t.Fatalf("width after reopen = %d", got.Spec().BSIWidth)
	}
	if got.Cardinality() != uint64(len(ref.values)) {
		t.Fatalf("cardinality = %d, want %d", got.Cardinality(), len(ref.values))
	}
	for probe := uint64(0); probe < 2000; probe += 137 {
		bit, _ := got.Equal(probe)
		assertIDs(t, bitmapIDs(t, bit), ref.equal(probe))
	}
	for id, want := range ref.values {
		v, present, err := got.Get(id)
		if err != nil || !present || v != want {
			t.Fatalf("reopen Get(%d) = %d,%v,%v want %d", id, v, present, err, want)
		}
	}
}

func TestBSIRepeatedWritesAndMissingValues(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	if _, ok, _ := index.Get(1); ok {
		t.Fatal("missing id reported present")
	}
	if err := index.Set(1, 1234); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := index.Set(1, 1234); err != nil {
			t.Fatal(err)
		}
	}
	if index.Cardinality() != 1 {
		t.Fatalf("cardinality = %d, want 1 after repeated identical writes", index.Cardinality())
	}
	// Deleting an absent value is a no-op.
	if err := index.Delete(999); err != nil {
		t.Fatal(err)
	}
	if index.Cardinality() != 1 {
		t.Fatalf("cardinality = %d after no-op delete", index.Cardinality())
	}
	// An empty/absent conjunction matches nothing.
	bit, _ := index.Equal(7)
	if !bit.IsEmpty() {
		t.Fatalf("absent value matched: %v", bitmapIDs(t, bit))
	}
}

func TestBSIStorageSize(t *testing.T) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		dir := t.TempDir()
		store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI32Namespace, width)}, NamespaceConfig{}, common.DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		index, _ := store.Lookup(testBSI32Namespace)
		values := bsiBenchValues(bsiDistUniform, width, 64_000)
		batch := index.NewBatch()
		for i, value := range values {
			if err := batch.BSIMove(BSIMove{ID: common.ValueID(i + 1), To: value, HasTo: true}); err != nil {
				t.Fatal(err)
			}
			if batch.Len() >= 4096 {
				if err := batch.Apply(); err != nil {
					t.Fatal(err)
				}
				if err := index.Barrier(); err != nil {
					t.Fatal(err)
				}
				if err := batch.Reset(); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !batch.Empty() {
			if err := batch.Apply(); err != nil {
				t.Fatal(err)
			}
			if err := index.Barrier(); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Vacuum(); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(dir, "namespaces-*.idx"))
		if len(files) != 1 {
			t.Fatalf("segment files = %v", files)
		}
		info, err := os.Stat(files[0])
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("width %d: %d ValueIDs -> %d segment bytes", width, len(values), info.Size())
	}
}

func TestBSIScratchOwnershipAcrossQueries(t *testing.T) {
	index := mustBSI(t, bsiNamespace(testBSI16Namespace, BSIWidth16))
	for i := 1; i <= 2000; i++ {
		if err := index.Set(common.ValueID(i), uint64(i%100)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := index.Equal(50)
	if err != nil {
		t.Fatal(err)
	}
	want := first.AppendTo(nil)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		lo := uint64(rng.Intn(100))
		if _, err := index.BSIRange(lo, lo+uint64(rng.Intn(20))); err != nil {
			t.Fatal(err)
		}
		if _, err := index.Less(uint64(rng.Intn(100))); err != nil {
			t.Fatal(err)
		}
		if _, err := index.Equal(uint64(rng.Intn(100))); err != nil {
			t.Fatal(err)
		}
	}
	// The earlier result must not alias live scratch: later queries recycle it.
	if got := first.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("earlier result mutated by later queries: got %v want %v", got, want)
	}
}

func TestBSIResultSurvivesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, BSIWidth16)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, ok := store.Lookup(testBSI16Namespace)
	if !ok {
		t.Fatal("namespace missing")
	}
	for i := 1; i <= 500; i++ {
		if err := index.Set(common.ValueID(i), uint64(i%64)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := index.Equal(10)
	if err != nil {
		t.Fatal(err)
	}
	want := result.AppendTo(nil)
	if len(want) == 0 {
		t.Fatal("no matches for the reference query")
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// Checkpoint may unmap the previous snapshot; a detached result must remain
	// valid because it owns its bytes.
	if got := result.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("result changed after checkpoint: got %v want %v", got, want)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBSICorruptSegmentRejected(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, BSIWidth16)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := store.Lookup(testBSI16Namespace)
	for i := 1; i <= 32; i++ {
		_ = index.Set(common.ValueID(i), uint64(i*3))
	}
	if err := store.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "namespaces-*.idx"))
	if err != nil || len(files) != 1 {
		t.Fatalf("segment files = %v (%v)", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xFF // corrupt the segment magic
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir, NamespaceConfig{}, common.DurabilitySync); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("open err = %v, want ErrCorruptSegment", err)
	}
}

func TestBSICorruptWALRejected(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, BSIWidth16)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := store.Lookup(testBSI16Namespace)
	for i := 1; i <= 4; i++ {
		if err := index.Set(common.ValueID(i), uint64(i*11)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "namespaces-*.wal"))
	if err != nil || len(files) != 1 {
		t.Fatalf("wal files = %v (%v)", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < namespaceWALHeaderSize+bsiWALRecordSize*2 {
		t.Fatalf("wal too short: %d", len(data))
	}
	// Corrupt the payload of the first (non-terminal) frame.
	data[namespaceWALHeaderSize+8] ^= 0xFF
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir, NamespaceConfig{}, common.DurabilitySync); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("open err = %v, want ErrCorruptWAL", err)
	}
}

func TestBSIMixedNamespaceStore(t *testing.T) {
	dir := t.TempDir()
	specs := []NamespaceSpec{
		enumNamespace(common.NamespaceTag),
		bsiNamespace(testBSI16Namespace, BSIWidth16),
		continuousNamespace(testFocalLength),
	}
	store, err := NewStore(dir, specs, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	tags, _ := store.Lookup(common.NamespaceTag)
	bsi, _ := store.Lookup(testBSI16Namespace)
	focal, _ := store.Lookup(testFocalLength)

	tagValue := EnumStringValue("cat")
	batch := tags.NewBatch()
	if err := batch.Add(Entry{ImageID: 1, Value: tagValue}, Entry{ImageID: 2, Value: tagValue}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := tags.Barrier(); err != nil {
		t.Fatal(err)
	}
	if err := bsi.Set(1, 42); err != nil {
		t.Fatal(err)
	}
	if err := bsi.Set(2, 7); err != nil {
		t.Fatal(err)
	}
	fbatch := focal.NewBatch()
	if err := fbatch.Add(Entry{ImageID: 1, Value: IntValue(50)}); err != nil {
		t.Fatal(err)
	}
	if err := fbatch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := focal.Barrier(); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(dir, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rtags, _ := reopened.Lookup(common.NamespaceTag)
	got, err := rtags.MatchValues(tagValue)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, got.AppendTo(nil), []common.ValueID{1, 2})
	rbsi, _ := reopened.Lookup(testBSI16Namespace)
	if v, ok, _ := rbsi.Get(1); !ok || v != 42 {
		t.Fatalf("bsi Get(1) after reopen = %d,%v", v, ok)
	}
	eq, _ := rbsi.Equal(7)
	assertIDs(t, eq.AppendTo(nil), []common.ValueID{2})
}

func TestBSIWALReplayWithoutCheckpoint(t *testing.T) {
	dir := t.TempDir()
	spec := bsiNamespace(testBSI16Namespace, BSIWidth16)
	store, err := NewStore(dir, []NamespaceSpec{spec}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := store.Lookup(testBSI16Namespace)
	ref := newRefBSI()
	// Interleave inserts, updates, and deletes; no explicit checkpoint, so
	// recovery must come entirely from WAL replay.
	for i := 1; i <= 200; i++ {
		value := uint64((i * 13) % 65536)
		_ = index.Set(common.ValueID(i), value)
		ref.set(common.ValueID(i), value)
	}
	for i := 1; i <= 200; i += 2 {
		_ = index.Set(common.ValueID(i), uint64(i))
		ref.set(common.ValueID(i), uint64(i))
	}
	for i := 1; i <= 200; i += 7 {
		_ = index.Delete(common.ValueID(i))
		ref.del(common.ValueID(i))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, _ := reopened.Lookup(testBSI16Namespace)
	if got.Cardinality() != uint64(len(ref.values)) {
		t.Fatalf("replayed cardinality = %d, want %d", got.Cardinality(), len(ref.values))
	}
	for id, want := range ref.values {
		v, present, _ := got.Get(id)
		if !present || v != want {
			t.Fatalf("replayed Get(%d) = %d,%v want %d", id, v, present, want)
		}
	}
}
