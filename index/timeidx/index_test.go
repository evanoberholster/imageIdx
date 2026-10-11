package timeidx

import (
	"cmp"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/common"
)

const testQuantum = uint64(1) << TimestampShift // benchmark fixture spacing, NOT encoding
const testNamespace common.Namespace = 9

func TestSoAColumnCapacityIsolation(t *testing.T) {
	times, ids := makeColumns(4)
	ids = append(ids, 10, 20, 30, 40)
	times = append(times, 1, 2, 3, 4, 5) // must grow, not overwrite IDs
	if !slices.Equal(ids, []uint64{10, 20, 30, 40}) {
		t.Fatal(ids)
	}
	if !slices.Equal(times, []uint64{1, 2, 3, 4, 5}) {
		t.Fatal(times)
	}
}

func mustNewIndex(t *testing.T, entries []Entry, offset uint64, cfg Config) *Index {
	t.Helper()
	i, err := NewMemory(Spec{Entries: entries, Offset: offset}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := i.Close(); err != nil {
			t.Error(err)
		}
	})
	return i
}
func equalIDSet(got, want []common.ValueID) bool {
	got = slices.Clone(got)
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func referenceRange(reference map[uint64]uint64, start, end uint64) []uint64 {
	var entries []Entry
	for id, ts := range reference {
		if ts >= start && ts < end {
			entries = append(entries, Entry{Timestamp: ts, ImageID: id})
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if c := cmp.Compare(a.Timestamp, b.Timestamp); c != 0 {
			return c
		}
		return cmp.Compare(a.ImageID, b.ImageID)
	})
	ids := make([]uint64, len(entries))
	for n, e := range entries {
		ids[n] = e.ImageID
	}
	return ids
}
func apply(t testing.TB, i *Index, ops ...Mutation) {
	t.Helper()
	b := NewBatch(len(ops))
	for _, op := range ops {
		if !b.Append(op) {
			t.Fatal("append")
		}
	}
	if err := i.Apply(b); err != nil {
		t.Fatal(err)
	}
}

func TestExactTimeAndEncoding(t *testing.T) {
	var last TimeKey
	for _, year := range []int{1900, 1969, 1970, 2000, 2100} {
		instant := time.Date(year, 12, 31, 23, 59, 59, 999999999, time.UTC)
		key, err := EncodeTime(instant)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeTime(key)
		if err != nil || !decoded.Equal(instant) || key <= last {
			t.Fatalf("roundtrip %v %v %v", instant, decoded, err)
		}
		last = key
	}
	if k, err := EncodeTime(time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)); k != 0 || err != nil {
		t.Fatal(k, err)
	}
	if _, err := EncodeTime(time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC)); err != ErrInvalidRange {
		t.Fatal(err)
	}
	if _, err := DecodeTime(TimeKey(^uint64(0))); err != ErrInvalidRange {
		t.Fatal(err)
	}
	i := mustNewIndex(t, []Entry{{Timestamp: 10, ImageID: 1}, {Timestamp: 11, ImageID: 2}}, 0, Config{})
	if got := i.Search(nil, 11, 12); !slices.Equal(got, []uint64{2}) {
		t.Fatal(got)
	}
	if i.EstimateRange(11, 12) != 1 {
		t.Fatal("estimate")
	}
}

func TestDuplicatesSplitAndMutationSemantics(t *testing.T) {
	entries := make([]Entry, 100000)
	for n := range entries {
		entries[n] = Entry{Timestamp: 42, ImageID: uint64(n)}
	}
	i := mustNewIndex(t, entries, 0, Config{MaxShardBytes: 1 << 20})
	if i.Metadata().BlockCount < 2 || len(i.Search(nil, 42, 43)) != len(entries) {
		t.Fatal("equal-key split")
	}
	apply(t, i, Delete(1, 99), Move(2, 999, 43), Insert(3, 44), Delete(3, 0), Insert(3, 45), Insert(3, 46))
	if len(i.Search(nil, 42, 43)) != 99997 || !slices.Equal(i.Search(nil, 43, 44), []uint64{2}) || !slices.Equal(i.Search(nil, 46, 47), []uint64{3}) {
		t.Fatal("last-write-wins")
	}
	apply(t, i, Insert(3, 46))
	if i.Len() != 99999 {
		t.Fatal("idempotence", i.Len())
	}
	_, err := NewMemory(Spec{Entries: []Entry{{ImageID: 1}, {Timestamp: 2, ImageID: 1}}}, Config{})
	if err != ErrDuplicateImageID {
		t.Fatal(err)
	}
}

func TestDifferentialBatches(t *testing.T) {
	for _, batchSize := range []int{1, 8, 32, 64} {
		t.Run(string(rune('A'+batchSize)), func(t *testing.T) {
			i := mustNewIndex(t, nil, 0, Config{MaxShardBytes: 256})
			reference := map[uint64]uint64{}
			rng := rand.New(rand.NewSource(7))
			for round := 0; round < 100; round++ {
				b := NewBatch(batchSize)
				for n := 0; n < batchSize; n++ {
					id, ts := uint64(rng.Intn(200)), uint64(rng.Intn(20))
					switch rng.Intn(3) {
					case 0:
						b.Append(Delete(id, 999))
						delete(reference, id)
					case 1:
						b.Append(Move(id, 999, ts))
						reference[id] = ts
					default:
						b.Append(Insert(id, ts))
						reference[id] = ts
					}
				}
				if err := i.Apply(b); err != nil {
					t.Fatal(err)
				}
				for start := uint64(0); start < 20; start += 3 {
					want := referenceRange(reference, start, start+3)
					got := i.Search(nil, start, start+3)
					if !slices.Equal(got, want) || i.EstimateRange(start, start+3) != uint64(len(want)) {
						t.Fatalf("round %d range %d: %v != %v", round, start, got, want)
					}
				}
			}
		})
	}
}

func TestAtomicSnapshotsConcurrent(t *testing.T) {
	i := mustNewIndex(t, nil, 0, Config{MaxShardBytes: 256})
	var stop atomic.Bool
	var readers sync.WaitGroup
	for n := 0; n < 4; n++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			buf := make([]uint64, 0, 64)
			for !stop.Load() {
				buf = i.Search(buf[:0], 0, 1000)
				if len(buf) != 0 && len(buf) != 64 {
					t.Error("batch prefix", len(buf))
					return
				}
			}
		}()
	}
	b := NewBatch(64)
	for round := 0; round < 100; round++ {
		b.Reset()
		for id := uint64(0); id < 64; id++ {
			b.Append(Insert(id, uint64(round)))
		}
		if err := i.Apply(b); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	readers.Wait()
}

func TestSearchBoundariesAndAllocations(t *testing.T) {
	i := mustNewIndex(t, []Entry{{Timestamp: 0, ImageID: 0}, {Timestamp: 1, ImageID: 2}, {Timestamp: ^uint64(0), ImageID: 3}}, 0, Config{MaxShardBytes: 16})
	buf := make([]uint64, 1, 10)
	buf[0] = 99
	if got := i.Search(buf, 0, ^uint64(0)); !slices.Equal(got, []uint64{99, 0, 2}) {
		t.Fatal(got)
	}
	if len(i.Search(nil, 2, 1)) != 0 || len(i.Search(nil, 1, 1)) != 0 {
		t.Fatal("empty")
	}
	if alloc := testing.AllocsPerRun(100, func() { buf = i.Search(buf[:1], 0, ^uint64(0)) }); alloc != 0 {
		t.Fatal(alloc)
	}
	apply(t, i, Delete(0, 0), Delete(2, 1), Delete(3, 0))
	if i.Len() != 0 {
		t.Fatal(i.Len())
	}
}

func TestBatchOwnershipAndConfig(t *testing.T) {
	i := mustNewIndex(t, nil, 0, Config{})
	if i.Apply(nil) != ErrUninitializedBatch || i.Apply(&Batch{}) != ErrUninitializedBatch {
		t.Fatal("uninitialized")
	}
	b := NewBatch(1)
	b.Append(Insert(1, 1))
	b.inFlight.Store(true)
	if b.Append(Insert(2, 2)) || b.Reset() || i.Apply(b) != ErrBatchInFlight {
		t.Fatal("ownership")
	}
	b.inFlight.Store(false)
	if _, err := NewMemory(Spec{}, Config{MaxShardBytes: 15}); err != ErrInvalidConfig {
		t.Fatal(err)
	}
	if _, err := NewMemory(Spec{}, Config{DeltaCapacity: -1}); err != ErrInvalidConfig {
		t.Fatal(err)
	}
}
