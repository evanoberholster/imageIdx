package timeidx

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/bench"
)

// timeMatrixSink accumulates measured result lengths so the compiler cannot
// elide the searches, opens, and recoveries driven by the BenchmarkTime
// matrix.
var timeMatrixSink int

const (
	// matrixSeed and matrixTailSeed keep every generated dataset
	// deterministic and reproducible across runs.
	matrixSeed     = int64(0x51D3)
	matrixTailSeed = int64(0x51D4)

	// matrixDeltaCapacity matches the delta capacity used by the existing
	// write-path benchmarks (BenchmarkDeltaCapacity, commit benchmarks).
	matrixDeltaCapacity = 4_096
	// matrixBatchSize is the Apply batch used to stage the recovery WAL
	// tail. It stays below 65535 so each batch fits one WAL frame.
	matrixBatchSize = 4_096
	// matrixCrossingBlockSize is deliberately small so even the smallest
	// matrix size spans several blocks and the crossing cases are real.
	matrixCrossingBlockSize = 256
	// matrixRecoveryDeltaCapacity sits above the per-block tail the
	// recovery benchmark writes, so reopen stays on the WAL replay and
	// reapply path instead of interleaving background rotations and
	// compactions.
	matrixRecoveryDeltaCapacity = 65_536
)

var (
	// matrixSearchConfig is the production-sized baseline: default block
	// size with the shared benchmark delta capacity.
	matrixSearchConfig = Config{DeltaCapacity: matrixDeltaCapacity, BlockSize: DefaultBlockSize}
	// matrixCrossingConfig shrinks blocks so 1K entries already span
	// multiple directory entries.
	matrixCrossingConfig = Config{DeltaCapacity: matrixDeltaCapacity, BlockSize: matrixCrossingBlockSize}
	// matrixOpenConfig sizes reopen delta allocation the same way the
	// open benchmark's store was created.
	matrixOpenConfig = Config{DeltaCapacity: matrixDeltaCapacity, BlockSize: DefaultBlockSize}
	// matrixRecoveryConfig keeps the replayed tail inside one active delta
	// per block for the default matrix sizes.
	matrixRecoveryConfig = Config{DeltaCapacity: matrixRecoveryDeltaCapacity, BlockSize: DefaultBlockSize}
)

// timeWindow is one deterministic timestamp search window.
type timeWindow struct {
	name       string
	start, end uint64
}

// BenchmarkTime is the deterministic, benchstat-stable timeidx matrix:
//
//	BenchmarkTime/Search/N=100000/Range=Small/Distribution=Uniform
//	BenchmarkTime/SearchCrossing/N=100000/Case=TwoBlocks
//	BenchmarkTime/Open/N=100000
//	BenchmarkTime/Recovery/N=100000
//
// Search spans every bench size, the small/medium/large/full ranges, and
// every harness distribution; SearchCrossing swaps distributions for
// block-boundary windows; Open and Recovery exercise persistent stores.
// Setup (dataset generation, index construction, dst sizing) always runs
// outside the timed region.
func BenchmarkTime(b *testing.B) {
	b.Run("Search", benchmarkTimeSearch)
	b.Run("SearchCrossing", benchmarkTimeSearchCrossing)
	b.Run("Open", benchmarkTimeOpen)
	b.Run("Recovery", benchmarkTimeRecovery)
}

// benchmarkTimeSearch walks the size x range x distribution matrix. Each
// (N, distribution) index is built once and shared by that distribution's
// four range leaves.
func benchmarkTimeSearch(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			for _, distribution := range bench.TimestampDistributions() {
				timestamps := bench.Timestamps(matrixSeed, n, distribution)
				index := newMatrixIndex(b, timestamps, matrixSearchConfig)
				for _, window := range matrixSearchWindows(timestamps) {
					b.Run(fmt.Sprintf("Range=%s/Distribution=%s", window.name, distribution), func(b *testing.B) {
						runTimeSearch(b, index, window)
					})
				}
				if err := index.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchmarkTimeSearchCrossing derives its windows from the live block
// directory instead of timestamp fractions, so each case is guaranteed to
// touch a known number of blocks. Uniform timestamps keep the block layout
// dense and non-degenerate at every size.
func benchmarkTimeSearchCrossing(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			timestamps := bench.Timestamps(matrixSeed, n, bench.TimeUniform)
			index := newMatrixIndex(b, timestamps, matrixCrossingConfig)
			for _, window := range matrixCrossingWindows(index.directory.Load().entries) {
				// Sanity: Miss must be empty, every other case must be
				// non-empty, or the derivation is wrong.
				if got := index.Search(nil, window.start, window.end); (window.name == "Miss") != (len(got) == 0) {
					b.Fatalf("case %s returned %d candidates", window.name, len(got))
				}
				b.Run(fmt.Sprintf("Case=%s", window.name), func(b *testing.B) {
					runTimeSearch(b, index, window)
				})
			}
			if err := index.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// benchmarkTimeOpen measures clean opens of an existing store. The store is
// checkpointed and closed once before the loop; each iteration then maps the
// published generation, decodes its directory, and opens its WAL. Setup heap
// and disk numbers are reported so benchstat tracks resident and on-disk
// growth alongside ns/op.
func benchmarkTimeOpen(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			entries := matrixEntries(bench.Timestamps(matrixSeed, n, bench.TimeUniform))
			heapBefore := bench.HeapBytes()
			writer, err := NewStore(dir, Spec{Entries: entries}, matrixOpenConfig, DurabilityOS)
			if err != nil {
				b.Fatal(err)
			}
			heapAfter := bench.HeapBytes()
			bench.ReportIndexSize(b, "TimeOpen", heapBefore, heapAfter, bench.DiskBytes(dir), n)
			if err := writer.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				opened, err := OpenStore(dir, matrixOpenConfig, DurabilityOS)
				if err != nil {
					b.Fatal(err)
				}
				timeMatrixSink += int(opened.Metadata().BaseCount)
				if err := opened.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchmarkTimeRecovery measures OpenStore replaying a WAL tail: a
// checkpointed base of N records is written first, then N more records are
// applied without another checkpoint, so every reopen must replay N frames
// and reapply them to the mapped base.
func benchmarkTimeRecovery(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			base := matrixEntries(bench.Timestamps(matrixSeed, n, bench.TimeUniform))
			tail := bench.Timestamps(matrixTailSeed, n, bench.TimeUniform)
			writer, err := NewStore(dir, Spec{Entries: base}, matrixRecoveryConfig, DurabilityOS)
			if err != nil {
				b.Fatal(err)
			}
			applyTimeTail(b, writer, tail, n)
			if err := writer.Close(); err != nil {
				b.Fatal(err)
			}
			b.Logf("TimeRecovery N=%d disk=%d bytes", n, bench.DiskBytes(dir))
			b.ReportAllocs()
			for b.Loop() {
				recovered, err := OpenStore(dir, matrixRecoveryConfig, DurabilityOS)
				if err != nil {
					b.Fatal(err)
				}
				timeMatrixSink += int(recovered.Metadata().BaseCount)
				if err := recovered.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// runTimeSearch measures one window against one prepared index. A seed search
// sizes dst so the timed path cannot grow it, and the candidate count is
// reported after the loop (B.Loop clears metrics reported before its first
// iteration) so benchstat sees how many IDs each window returns.
func runTimeSearch(b *testing.B, index *Index, window timeWindow) {
	seed := index.Search(nil, window.start, window.end)
	dst := make([]common.ValueID, 0, len(seed)+1)
	b.ReportAllocs()
	for b.Loop() {
		dst = index.Search(dst[:0], window.start, window.end)
		timeMatrixSink += len(dst)
	}
	b.ReportMetric(float64(len(seed)), "results")
}

// matrixSearchWindows derives deterministic bounds from the generated
// timestamps: Small is a narrow window anchored at the median timestamp so it
// always returns a few IDs, Medium is the central half of the [min, max]
// span, Large is that span with a 1/32 margin at each end, and Full is the
// entire uint64 domain.
func matrixSearchWindows(timestamps []uint64) []timeWindow {
	minimum, maximum := slices.Min(timestamps), slices.Max(timestamps)
	span := maximum - minimum
	center := minimum + span/2
	anchor := timestamps[len(timestamps)/2]
	small := max(span/4_096, 1)
	return []timeWindow{
		{name: "Small", start: anchor, end: anchor + small},
		{name: "Medium", start: center - span/4, end: center + span/4},
		{name: "Large", start: minimum + span/32, end: maximum - span/32},
		{name: "Full", start: 0, end: math.MaxUint64},
	}
}

// matrixCrossingWindows expresses each case through block directory metadata.
// Blocks tile the marker space contiguously, so a window that starts on one
// entry's StartTime and ends on a later entry's StartTime touches exactly the
// entries in between:
//
//	Miss       a pre-data marker bucket holding no records
//	OneBlock   one middle block's marker span
//	TwoBlocks  two adjacent middle blocks
//	ManyBlocks the middle 3/4 of the directory
//	Whole      the full uint64 domain
//
// Indexes used here are built with offset zero, matching
// timestampForBoundary's offset parameter.
func matrixCrossingWindows(entries []blockEntry) []timeWindow {
	count := len(entries)
	middle := count / 2
	twoLeft := count / 4
	twoRight := min(twoLeft+2, count)
	manyLeft := count / 8
	manyRight := count - manyLeft
	if manyRight == count && manyLeft == 0 && manyRight > 1 {
		// Keep ManyBlocks distinct from Whole whenever at least one
		// whole block can be dropped from the window.
		manyRight--
	}
	// The exclusive end is the next block's StartTime; only a window that
	// runs through the final block may end at markerSpaceEnd.
	manyEnd := entries[count-1].meta.EndTime
	if manyRight < count {
		manyEnd = entries[manyRight].meta.StartTime
	}
	missStart, missEnd := matrixMissWindow(entries)
	return []timeWindow{
		{name: "Miss", start: missStart, end: missEnd},
		{name: "OneBlock", start: entries[middle].meta.StartTime, end: entries[middle].meta.EndTime},
		{name: "TwoBlocks", start: entries[twoLeft].meta.StartTime, end: entries[twoRight-1].meta.EndTime},
		{name: "ManyBlocks", start: entries[manyLeft].meta.StartTime, end: manyEnd},
		{name: "Whole", start: 0, end: math.MaxUint64},
	}
}

// matrixMissWindow returns a zero-result window below the first record. The
// fallback handles data that starts at marker zero by moving above the highest
// record instead.
func matrixMissWindow(entries []blockEntry) (uint64, uint64) {
	firstBase := entries[0].block.view.Load().base
	if first := uint64(firstBase.markers[0]); first != 0 {
		return 0, timestampForBoundary(first, 0)
	}
	lastBase := entries[len(entries)-1].block.view.Load().base
	last := uint64(lastBase.markers[lastBase.len()-1])
	return timestampForBoundary(last+1, 0), timestampForBoundary(last+2, 0)
}

// newMatrixIndex builds an in-memory index from deterministic timestamps,
// assigning each record a unique ImageID.
func newMatrixIndex(b *testing.B, timestamps []uint64, config Config) *Index {
	b.Helper()
	index, err := NewMemory(Spec{Entries: matrixEntries(timestamps)}, config)
	if err != nil {
		b.Fatal(err)
	}
	return index
}

// matrixEntries pairs each timestamp with a unique ImageID.
func matrixEntries(timestamps []uint64) []Entry {
	entries := make([]Entry, len(timestamps))
	for i, timestamp := range timestamps {
		entries[i] = Entry{Timestamp: timestamp, ImageID: common.ValueID(i + 1)}
	}
	return entries
}

// applyTimeTail stages tail as Insert mutations with IDs offset past the
// base, applying matrixBatchSize-sized batches and waiting out backpressure so
// setup always completes. It runs outside every timed region.
func applyTimeTail(b *testing.B, index *Index, tail []uint64, idBase int) {
	b.Helper()
	batch := NewBatch(matrixBatchSize)
	for start := 0; start < len(tail); start += matrixBatchSize {
		end := min(start+matrixBatchSize, len(tail))
		if !batch.Reset() {
			b.Fatal("batch still in flight")
		}
		for i := start; i < end; i++ {
			if !batch.Append(Insert(common.ValueID(idBase+i+1), tail[i])) {
				b.Fatalf("append mutation %d failed", i)
			}
		}
		for {
			err := index.Apply(batch)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrBackpressure) {
				b.Fatal(err)
			}
			runtime.Gosched()
		}
	}
}
