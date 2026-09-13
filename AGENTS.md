# imageIdx Agent Instructions

imageIdx is a performance-oriented Go image indexing and search library.

## Repository navigation

Use SigMap as the default repository retrieval layer:

```sh
sigmap ask "<task or question>"
```

Start with the highest-ranked 2–5 files or symbols returned by SigMap. Read
more only when the implementation requires following a dependency. For a
topic-oriented lookup, use:

```sh
sigmap --query "<topic>"
```

Skip discovery when the user names the exact file and symbol and the change is
demonstrably local. Do not perform repository-wide grep, glob, or source reads
before discovery unless the initial SigMap result is insufficient.

## Engineering priorities

Prioritize, in order:

1. Correctness
2. Predictable low allocation
3. Query/read performance
4. Simple architecture
5. Storage efficiency

Prefer immutable reader-visible structures, lock-free readers, serialized
writes/publication, reusable scratch memory, zero-copy mmap reads, and SIMD
behind internal kernels. Avoid speculative architectural redesigns.

Keep package dependencies one-way: index packages may use shared root,
`roar`, and `internal/indexutil` mechanisms; `search` may combine indexes, but
index packages must not import `search`.

## Implementation workflow

1. Locate the smallest relevant surface with SigMap.
2. Read the relevant implementation and tests.
3. Make the smallest correct change.
4. Run focused tests or benchmarks.
5. Run `sigmap validate` after source or configuration changes.

Use a new OpenCode session for unrelated features or index subsystems. Keep a
session for a feature, its direct fixes, and benchmark tuning.

Do not edit generated context files unless explicitly required; regenerate them
through SigMap instead.

## Validation

Run the smallest relevant checks first. Before handing off a broad change, run:

```sh
go build ./...
go test ./...
go test -race ./...
sigmap validate
```


## Auto-generated signatures
<!-- Updated by gen-context.js -->
# Code signatures

## SigMap commands

| When | Command |
|------|---------|
| Before answering a question about code | `sigmap ask "<your question>"` |
| To rank files by topic | `sigmap --query "<topic>"` |
| After changing config or source dirs | `sigmap validate` |
| To verify an AI answer is grounded | `sigmap judge --response <file>` |

Always run `sigmap ask` (or `sigmap --query`) before searching for files relevant to a task.

## changes (last 5 commits — 3 hours ago)
```
bitmapidx/backend_benchmark_test.go           ~benchBackendApply
bitmapidx/search.go                           ~allDeltaFree  ~Intersect  ~filterCandidatesGuarded
bitmapidx/segment.go                          ~openSegment
bitmapidx/storage.go                          ~NewPersistent  ~segmentPath  ~readBitmapManifest
bitmapidx/wal.go                              ~replayBitmapWAL
cmd/imageidx/main.go                          ~newDemoService
internal/indexutil/wait.go                    +WaitForCondition
roar/bitmap_api_test.go                       +TestEmptyOperandsShortCircuit  ~TestBitmapZeroValue
roar/builder_test.go                          +TestTinyOpsAvoidDenseScratch  ~TestBuilderResetReinitializesScratchWhenNeeded
roar/concurrent.go                            +shouldParallelize  ~combineBuilders  ~runParallel  ~FastParOr
roar/container_array.go                       +shouldGallop  +lowerBoundFrom  +intersectArrays  +unionArrays
roar/delta.go                                 +NewDeltaBuilder  +NewDeltaBuilderFrom  +bitmapFromUnique
roar/internal/bitmapkernel/avx2_amd64.go      +andAVX2FixedAsm  +orAVX2FixedAsm  +andNotAVX2FixedAsm  +applyAVX2FixedAsm
roar/internal/bitmapkernel/benchmark_test.go  +benchContainers  +BenchmarkAnd  +BenchmarkOr  +BenchmarkAndNot
roar/internal/bitmapkernel/correctness_test.go +refAnd  +refOr  +refAndNot  +refIntersects
roar/keys.go                                  ~findKey
search/benchmark_test.go                      ~BenchmarkCombinedSearch
search/clause.go                              +ExactTimestamp  +During  +WithinCells  ~ExactTimestamp
search/execute.go                             +truncateLimit  ~Search
search/plan.go                                +geoFilter  +smallestBitmap  +collectBitmaps  +bitmapEstimate
search/search_test.go                         +TestBitmapOnlyCountMatchesSearch  +TestGeoStageFiltersSmallCandidatesAndCollectsLarge  ~TestCompileAndSearchCombinedQuery  ~TestWithinCellsAcceptsCellIDsAndCoarseCells
search/service_stress_test.go                 +buildStressFixture  +checkSortedUnique  +runLoad  +logLoadResult
search/service.go                             +NewService
```

## bitmapidx

### bitmapidx/benchmark_test.go
```
func BenchmarkContains(b *testing.B)  :9-20
func BenchmarkAnd4(b *testing.B)  :22-36
func BenchmarkApplyBatch1000(b *testing.B)  :38-49
func BenchmarkAppendTo(b *testing.B)  :51-63
func BenchmarkOr4(b *testing.B)  :65-79
func BenchmarkAnd2(b *testing.B)  :81-95
func BenchmarkOr2(b *testing.B)  :97-111
func BenchmarkFilter(b *testing.B)  :113-132
func spacedIDs(count, step, shift int) → []uint64  :140-147
func benchmarkBackendBuild(b *testing.B)  :149-155
func benchmarkBackendContains(b *testing.B)  :157-164
func benchmarkBackendAppendTo(b *testing.B)  :166-174
func backendFourSets(count int) → []roar.Bitmap  :176-182
func benchmarkBackendAnd4(b *testing.B)  :184-190
func benchmarkBackendOr4(b *testing.B)  :192-198
func benchmarkBackendApply(b *testing.B)  :200-203
func benchmarkBackendApplySmall(b *testing.B)  :205-208
func benchBackendApply(b *testing.B, base, adds, removes []uint64)  :210-221
func BenchmarkBackendBuild(b *testing.B)  :223-223
func BenchmarkBackendContains(b *testing.B)  :224-224
func BenchmarkBackendAppendTo(b *testing.B)  :225-225
func BenchmarkBackendAnd4(b *testing.B)  :226-226
func BenchmarkBackendOr4(b *testing.B)  :227-227
func BenchmarkBackendApply(b *testing.B)  :228-228
func BenchmarkBackendApplySmall(b *testing.B)  :229-229
… +3 more signatures
```

### bitmapidx/index.go
```
type Index struct  :20-29  # Index is one logical bitmap predicate: an immutable set of I
func New(spec Spec, config Config) → (*Index, error)  :32-38  # New constructs an index from its stable metadata and initial
func newIndex(spec Spec, config Config, readers *indexutil.ReaderTracker) → (*Index, error)  :40-57
func newIndexFromView(view *indexView, config Config, readers *indexutil.ReaderTracker) → (*Index, error)  :59-66
func (i) Metadata() → Metadata  :69-78  # Metadata returns the immutable public portion of the current
func (i) PendingCompactions() → int64  :81-81  # PendingCompactions reports the number of in-flight backgroun
func (i) WaitIdle(ctx context.Context) → error  :85-87  # WaitIdle blocks until all background compactions have draine
func (i) Contains(id ImageID) → bool  :91-97  # Contains reports whether id is a member of the effective set
func (i) AppendTo(dst []ImageID) → []ImageID  :101-108  # AppendTo appends the index's effective IDs to dst in ascendi
func (i) Apply(mutation Mutation) → error  :125-164  # Apply atomically publishes one mutation
func (i) publishLocked(old *indexView, base roar.Bitmap, frozen, active *bitmapDelta) → *indexView  :169-184  # publishLocked builds the replacement view and publishes it w
func composeActive(view *indexView, mutation Mutation) → *bitmapDelta  :190-221  # composeActive canonicalizes the mutation against the current
func containsWithoutActive(view *indexView, id ImageID) → bool  :225-230  # containsWithoutActive reports id's membership below the Acti
func deltaLen(delta *bitmapDelta) → int  :232-237
func applyMutation(builder *roar.DeltaBuilder, mutation Mutation)  :240-247  # applyMutation applies raw additions and removals to a DeltaB
func compactBase(base roar.Bitmap, delta *bitmapDelta) → roar.Bitmap  :251-255  # compactBase folds a delta into a replacement Base without mu
func (i) scheduleCompaction(frozen *bitmapDelta)  :260-283  # scheduleCompaction folds the Frozen delta into the Base on a
func (i) Compact() → error  :287-301  # Compact synchronously merges both deltas into a replacement 
func (i) Close() → error  :305-312  # Close marks the index closed, waits for background compactio
```

### bitmapidx/search.go
```
type readToken struct  :8-11
func And(indexes ...*Index) → roar.Bitmap  :18-47  # And returns the logical intersection of the supplied immutab
func Or(indexes ...*Index) → roar.Bitmap  :52-80  # Or returns the logical union
func Count(indexes ...*Index) → uint64  :87-117  # Count returns the number of images present in every index wi
func Filter(dst, candidates []ImageID, indexes ...*Index) → []ImageID  :123-132  # Filter appends candidates present in every index to dst
func filterGuarded(dst, candidates []ImageID, indexes []*Index) → []ImageID  :134-191
func acquireReaders(dst []readToken, indexes []*Index) → []readToken  :195-213  # acquireReaders registers with each distinct tracker, dedupli
func releaseReaders(tokens []readToken)  :216-220  # releaseReaders unregisters in reverse acquisition order
```

### bitmapidx/segment.go
```
type segmentEntry struct  :23-33
type segment struct  :38-44  # segment is one immutable on-disk generation of the index col
func writeSegment(path string, generation uint64, indexes []*Index) → error  :48-118  # writeSegment writes all effective bitmap states into one ord
func openSegment(path string, config Config) → (*segment, error)  :157-210
func (s) close() → error  :239-253
func encodeSegmentEntry(dst []byte, e segmentEntry, checksum uint32)  :255-267
func decodeSegmentEntry(src []byte) → (segmentEntry, uint32)  :269-277
```

### bitmapidx/storage.go
```
type Store struct  :29-40  # Store owns a persistent collection of bitmap indexes backed 
type bitmapManifest struct  :42-46
func NewPersistent(dir string, specs []Spec, config Config, durability Durability) → (*Store, error)  :49-86  # NewPersistent creates and checkpoints a persistent bitmap st
func OpenPersistent(dir string, config Config, durability Durability) → (*Store, error)  :89-134  # OpenPersistent opens a checkpoint and replays its WAL tail
func (s) Lookup(key Key) → (*Index, bool)  :137-137  # Lookup returns an index by logical predicate key
func (s) LookupID(indexID uint64) → (*Index, bool)  :140-140  # LookupID returns an index by its stable numeric identifier
func (s) Indexes() → []*Index  :143-143  # Indexes returns indexes in physical storage order
func (s) Len() → int  :146-146  # Len returns the number of indexes in the store
func (s) Apply(key Key, mutation Mutation) → error  :149-163  # Apply appends a mutation to the WAL and publishes it to the 
func (s) Add(key Key, ids []ImageID) → error  :165-167
func (s) Remove(key Key, ids []ImageID) → error  :169-171
func (s) Checkpoint() → error  :173-201
func (s) Vacuum() → error  :205-211  # Vacuum checkpoints live bases into physical key order and re
func (s) Close() → error  :213-234
func segmentPath(dir string, generation uint64) → string  :236-238
func writeBitmapManifest(dir string, manifest bitmapManifest) → error  :240-247
func readBitmapManifest(path string) → (bitmapManifest, error)  :249-262
```

### bitmapidx/view.go
```
type bitmapDelta struct  :12-15  # bitmapDelta is an immutable set of recent corrections applie
type indexView struct  :72-86  # indexView is one immutable, atomically published snapshot of
func newBitmapDelta(additions, removals []ImageID) → *bitmapDelta  :20-34  # newBitmapDelta builds a delta from raw ID slices, canonicali
func (d) len() → int  :38-43  # len reports the total number of corrected IDs in the delta
func (d) state(id ImageID) → (present, touched bool)  :49-60  # state reports whether the delta names id, and if so the desi
func (v) contains(id ImageID) → bool  :90-101  # contains reports whether id is in the effective set, walking
func (v) hasDeltas() → bool  :105-107  # hasDeltas reports whether the view carries any reader-visibl
func (v) appendTo(dst []ImageID) → []ImageID  :112-139  # appendTo appends the effective IDs in ascending order withou
func (v) effectiveBitmap() → roar.Bitmap  :144-150  # effectiveBitmap materializes the fully merged set as a new i
func (v) effectiveBuilder() → *roar.DeltaBuilder  :155-163  # effectiveBuilder clones the Base into a writer-owned DeltaBu
func applyDelta(dst *roar.DeltaBuilder, delta *bitmapDelta)  :167-172  # applyDelta applies a delta's additions and removals to a bui
```

### bitmapidx/wal.go
```
type bitmapWAL struct  :16-18
type walMutation struct  :20-24
func openBitmapWAL(path string) → (*bitmapWAL, error)  :26-32
func (w) append(key Key, indexID uint64, mutation Mutation, durability Durability) → error  :34-61
func replayBitmapWAL(path string, offset int64) → ([]walMutation, error)  :63-103
```

## cmd

### cmd/imageidx/main.go
```
type searchRequest struct  :28-32
type searchResponse struct  :34-36
type demoService struct  :38-44
func main()  :46-65
func runOnce(output io.Writer) → error  :69-85  # runOnce demonstrates the convenience path that compiles and 
func runServer(ctx context.Context, address string) → error  :90-121  # runServer demonstrates a long-lived HTTP service
func newDemoService() → (*demoService, error)  :123-178
func (service) Close() → error  :180-188
func (service) searchOnce(request searchRequest) → ([]imageidx.ImageID, error)  :190-204
func (service) handler() → http.Handler  :206-223
func parseSearchRequest(request *http.Request) → (searchRequest, error)  :225-249
```

## internal

### internal/indexutil/wait.go
```
func WaitForCondition(ctx context.Context, done func() → bool) error  :15-26  # WaitForCondition polls done every 100 microseconds until it 
```

## roar

### roar/bitmap_api_test.go
```
func TestFromSorted(t *testing.T)  :8-26
func TestFromSortedEmpty(t *testing.T)  :28-39
func TestFromSortedErrors(t *testing.T)  :41-63
func TestBitmapZeroValue(t *testing.T)  :65-97
func TestEmptyOperandsShortCircuit(t *testing.T)  :99-137
```

### roar/builder_test.go
```
func TestSortedBuilder(t *testing.T)  :9-40
func TestSortedBuilderResetClearsUnfinishedBitmap(t *testing.T)  :42-71
func TestSortedBuilderAddAfterFinishRequiresReset(t *testing.T)  :73-96
func TestArrayToBitmapThreshold(t *testing.T)  :98-127
func TestBuilderCompactAndReset(t *testing.T)  :129-158
func TestBuilderResetReinitializesScratchWhenNeeded(t *testing.T)  :160-195
func is(err error, target error) → bool  :197-199
func TestTinyOpsAvoidDenseScratch(t *testing.T)  :201-227
```

### roar/compact.go
```
func assemble(keys []uint64, offsets []uint64, data []byte, cardinality uint64) → []byte  :8-14  # assemble writes the header, key table, and container payload
func writeHeaderKeyTable(out []byte, keys, offsets []uint64, cardinality uint64)  :19-32  # writeHeaderKeyTable writes the header and key table into the
func (b) Compact() → []byte  :37-39  # Compact assembles the accumulated containers into a compact 
func (b) CompactInto(reuse []byte) → []byte  :44-55  # CompactInto assembles the accumulated containers into reuse'
func (b) finishResult(reuse []byte, owned bool) → []byte  :60-65  # finishResult returns the accumulated result
```

### roar/concurrent.go
```
func keyBoundaries(numGo int) → []uint64  :19-30  # keyBoundaries partitions the key domain into numGo ascending
func keyRange(entries []keyEntry, lo, hi uint64) → (int, int)  :34-38  # keyRange returns the half-open index range [start, end) of e
func combineBuilders(builders []*Builder) → []byte  :42-62  # combineBuilders concatenates builders whose key sets are dis
func shouldParallelize(totalKeys, numGo int) → bool  :71-73  # shouldParallelize reports whether numGo-way parallel executi
func runParallel(numGo int, fn func(dst *Builder, lo, hi uint64) → ) Bitmap  :77-102  # runParallel executes fn over numGo contiguous key ranges and
func AndConc(a, b Bitmap, maxConcurrency int) → Bitmap  :107-114  # AndConc returns the intersection of a and b, computing conta
func OrConc(a, b Bitmap, maxConcurrency int) → Bitmap  :118-125  # OrConc returns the union of a and b, computing container pai
func AndNotConc(a, b Bitmap, maxConcurrency int) → Bitmap  :129-136  # AndNotConc returns a minus b, computed in parallel across up
func FastParOr(numGo int, bitmaps ...Bitmap) → Bitmap  :140-161  # FastParOr returns the union of bitmaps, computing it in para
func andIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64)  :164-180  # andIntoRange is AndInto restricted to keys in [lo, hi)
func orIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64)  :183-201  # orIntoRange is OrInto restricted to keys in [lo, hi)
func andNotIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64)  :204-222  # andNotIntoRange is AndNotInto restricted to keys in [lo, hi)
func orManyIntoRange(dst *Builder, lo, hi uint64, bitmaps ...Bitmap)  :225-254  # orManyIntoRange is the N-way union restricted to keys in [lo
func minBoundedKey(entries [][]keyEntry, positions, ends []int) → (uint64, bool)  :256-269
func orContainers(dst, fold *Builder, buffer *[]byte, key uint64, containers [][]byte)  :274-296  # orContainers ORs the containers that share a key into dst as
```

### roar/container_array.go
```
func shouldGallop(small, large []uint16) → bool  :14-16  # shouldGallop reports whether small is small enough relative 
func lowerBoundFrom(values []uint16, from int, target uint16) → int  :21-43  # lowerBoundFrom returns the index of the first element >= tar
func arrayContains(values []uint16, x uint16) → bool  :48-70  # arrayContains reports whether a sorted array contains x
func intersectArrays(dst *[]uint16, a, b []uint16)  :73-103  # intersectArrays writes the sorted intersection of a and b to
func intersectArraysCount(a, b []uint16) → int  :106-135  # intersectArraysCount counts the sorted intersection of a and
func unionArrays(dst *[]uint16, a, b []uint16)  :138-165  # unionArrays writes the sorted union of a and b to *dst
func unionGallop(dst *[]uint16, small, large []uint16)  :169-183  # unionGallop writes the union of small and large, where large
func unionArraysCount(a, b []uint16) → int  :187-189  # unionArraysCount counts the sorted union of a and b without 
func differenceArrays(dst *[]uint16, a, b []uint16)  :192-220  # differenceArrays writes a minus b to *dst
func differenceArraysCount(a, b []uint16) → int  :223-225  # differenceArraysCount counts a minus b without materializing
```

### roar/contains.go
```
func (v) Contains(id uint64) → bool  :6-22  # Contains reports whether id is present
```

### roar/delta.go
```
type DeltaBuilder struct  :33-41  # DeltaBuilder is a writer-owned builder that accumulates raw 
func NewDeltaBuilder() → *DeltaBuilder  :44-50  # NewDeltaBuilder returns an empty DeltaBuilder
func NewDeltaBuilderFrom(b Bitmap) → *DeltaBuilder  :54-60  # NewDeltaBuilderFrom returns a DeltaBuilder seeded with b
func (m) AddMany(ids []uint64)  :64-70  # AddMany stages additions of ids
func (m) Remove(id uint64)  :73-79  # Remove stages a removal of id
func (m) Or(b Bitmap)  :82-84  # Or unions b into the builder's current bitmap
func (m) And(b Bitmap)  :87-89  # And intersects b with the builder's current bitmap
func (m) AndNot(b Bitmap)  :92-94  # AndNot subtracts b from the builder's current bitmap
func (m) ApplyDelta(add, remove Bitmap)  :99-107  # ApplyDelta folds additions minus removals into the builder i
func (m) foldSet(operand Bitmap, keyCount, dataBytes int, op func(dst *Builder, a, b Bitmap) → )  :112-117  # foldSet applies one whole-set operation with the shared resu
func (m) Reset()  :121-127  # Reset clears the builder while retaining all allocated capac
func (m) Freeze() → Bitmap  :132-135  # Freeze materializes pending additions/removals and returns t
func (m) flush()  :139-154  # flush merges pending additions (then removals) into the curr
func bitmapFromUnique(ids []uint64) → Bitmap  :158-173  # bitmapFromUnique sorts ids in place, removes duplicates and 
```

### roar/filter_sorted.go
```
func (b) FilterSorted(dst, candidates []uint64) → []uint64  :9-23  # FilterSorted appends the candidates present in b to dst
```

### roar/internal/bitmapkernel/avx2_amd64.go
```
func andAVX2FixedAsm(dst, a, b []uint64) → uint32 func orAVX2FixedAsm(dst  :11-48
func andAVX2(dst, a, b []uint64) → uint32  :50-67
func orAVX2(dst, a, b []uint64) → uint32  :69-86
func andNotAVX2(dst, a, b []uint64) → uint32  :88-105
func intersectsAVX2(a, b []uint64) → bool  :107-124
func andCardinalityAVX2(a, b []uint64) → uint64  :126-141
func orCardinalityAVX2(a, b []uint64) → uint64  :143-158
func andNotCardinalityAVX2(a, b []uint64) → uint64  :160-175
func cardinalityAVX2(words []uint64) → uint64  :177-192
func cardinalityPrefixAVX2(words []uint64, wordCount int) → uint64  :194-201
func andManyAVX2(dst []uint64, inputs ...[]uint64) → uint32  :203-234
func andMany5AVX2(dst, a, b, c, d, e []uint64) → uint32  :236-254
func andMany6AVX2(dst, a, b, c, d, e, f []uint64) → uint32  :256-275
func andMany7AVX2(dst, a, b, c, d, e, f, g []uint64) → uint32  :277-297
func andMany8AVX2(dst, a, b, c, d, e, f, g, h []uint64) → uint32  :299-320
func andMany3AVX2(dst, a, b, c []uint64) → uint32  :322-339
func andMany4AVX2(dst, a, b, c, d []uint64) → uint32  :341-358
func andMany3CardinalityAVX2(a, b, c []uint64) → uint64  :360-365
func andMany4CardinalityAVX2(a, b, c, d []uint64) → uint64  :367-372
func applyAVX2(dst, base, add, remove []uint64) → uint32  :374-393
```

### roar/internal/bitmapkernel/benchmark_test.go
```
func benchContainers(rng *rand.Rand, dense bool) → (a, b, c, dst []uint64)  :9-32
func BenchmarkAnd(b *testing.B)  :34-49
func BenchmarkOr(b *testing.B)  :51-66
func BenchmarkAndNot(b *testing.B)  :68-83
func BenchmarkIntersects(b *testing.B)  :85-100
func BenchmarkAndMany(b *testing.B)  :102-117
func BenchmarkAndMany4(b *testing.B)  :119-142
func BenchmarkAndManyCardinality(b *testing.B)  :144-163
func BenchmarkCardinalityPrefix(b *testing.B)  :165-176
func BenchmarkAndManyVariadic4(b *testing.B)  :178-189
func BenchmarkAndManyVariadic3(b *testing.B)  :191-198
func BenchmarkAndManyVariadic5To8(b *testing.B)  :200-218
func BenchmarkApply(b *testing.B)  :220-235
func BenchmarkScalarBaseline(b *testing.B)  :239-246  # BenchmarkScalarBaseline measures the scalar backend directly
```

### roar/internal/bitmapkernel/correctness_test.go
```
func refAnd(dst, a, b []uint64) → uint32  :13-21
func refOr(dst, a, b []uint64) → uint32  :23-31
func refAndNot(dst, a, b []uint64) → uint32  :33-41
func refIntersects(a, b []uint64) → bool  :43-50
func makeShape(shape string, rng *rand.Rand) → []uint64  :55-86  # makeShape returns a test container matching the named shape
func TestKernelsAgainstReference(t *testing.T)  :88-146
func TestFusedKernelsAgainstReference(t *testing.T)  :148-187
func TestGenericAndManyAgainstReference(t *testing.T)  :189-209
func refOrInto(a, b []uint64) → []uint64  :211-215
func refAndInto(a, b []uint64) → []uint64  :217-221
```

### roar/iterator.go
```
type Iterator struct  :15-30  # Iterator walks a bitmap's IDs in ascending order without mat
func (v) Iterator() → Iterator  :35-38  # Iterator walks a bitmap's IDs in ascending order without mat
func (v) iteratorRange(start, end int) → Iterator  :42-44  # iteratorRange returns an iterator restricted to the key entr
func (b) NewRangeIterators(numRanges int) → []Iterator  :49-71  # NewRangeIterators partitions the bitmap into numRanges conti
func (it) advance() → bool  :75-122  # advance moves to the next non-empty position: the next non-z
func (it) Next() → bool  :125-144  # Next advances to the following ID and reports whether one ex
func (it) Value() → uint64  :147-149  # Value returns the ID produced by the most recent successful 
func (v) AppendTo(dst []uint64) → []uint64  :155-193  # AppendTo appends all IDs in ascending order to dst
```

### roar/keys.go
```
func (v) findEntry(key uint64) → (keyEntry, bool)  :8-15  # findEntry locates the entry for an upper-48-bit key
func findKey(entries []keyEntry, key uint64) → (int, bool)  :20-43  # findKey locates key in entries
```

### roar/setops.go
```
func And(a, b Bitmap) → Bitmap  :7-15  # And returns the intersection of a and b as a new Bitmap
func Or(a, b Bitmap) → Bitmap  :19-30  # Or returns the union of a and b as a new Bitmap
func AndNot(a, b Bitmap) → Bitmap  :34-45  # AndNot returns a minus b as a new Bitmap
func AndMany(bitmaps ...Bitmap) → Bitmap  :49-72  # AndMany returns the N-way intersection of bitmaps as a new B
```

## search

### search/benchmark_test.go
```
func BenchmarkCombinedSearch(b *testing.B)  :14-69
func BenchmarkGeoCollectSearch(b *testing.B)  :75-118  # BenchmarkGeoCollectSearch forces the geo stage down the coll
```

### search/clause.go
```
type bitmapClause struct  :22-24
type timestampClause struct  :33-38
type geoClause struct  :73-76
type Clause interface  :18-20  # Clause is one conjunctive search restriction
  searchClause()  :19-19
type TimestampOption interface  :43-45  # TimestampOption configures a timestamp clause
  applyTimestamp(*timestampClause)  :44-44
func AllBitmaps(indexes ...*bitmapidx.Index) → Clause  :29-31  # AllBitmaps requires every supplied bitmap to contain an imag
func (option) applyTimestamp(clause *timestampClause)  :49-51
func ExactTimestamp(lookup timeidx.ExactTimestampFunc) → TimestampOption  :54-58  # ExactTimestamp verifies lossy timestamp candidates against c
func During(index *timeidx.Index, start, end uint64, options ...TimestampOption) → Clause  :63-71  # During restricts images to the half-open timestamp range [st
func WithinCells(index *geoidx.Index, cellIDs ...uint64) → (Clause, error)  :84-93  # WithinCells restricts images to the union of the supplied S2
```

### search/execute.go
```
func Search(dst []ImageID, clauses ...Clause) → ([]ImageID, error)  :13-19  # Search compiles clauses and immediately appends their sorted
func (plan) Search(dst []ImageID, workspace *Workspace) → []ImageID  :23-26  # Search compiles clauses and immediately appends their sorted
func (plan) SearchContext(ctx context.Context, dst []ImageID, workspace *Workspace) → ([]ImageID, error)  :29-31  # SearchContext is Search with cancellation checked between st
func (plan) SearchLimit(dst []ImageID, workspace *Workspace, limit int) → []ImageID  :35-41  # SearchLimit appends at most limit sorted, unique matches to 
func (plan) Count(workspace *Workspace) → int  :45-48  # Count returns the number of sorted, unique matches without r
func (plan) CountContext(ctx context.Context, workspace *Workspace) → (int, error)  :51-69  # CountContext is Count with cancellation checked between stag
func (plan) Iterate(workspace *Workspace) → iter.Seq[ImageID]  :75-94  # Iterate yields sorted, unique matches
func (plan) searchInto(ctx context.Context, dst []ImageID, workspace *Workspace, limit int) → ([]ImageID, error)  :96-162
func truncateLimit(dst []ImageID, start int, limit int) → []ImageID  :164-169
func collectStage(dst []ImageID, ws *Workspace, st *stage) → []ImageID  :175-186  # collectStage appends one stage's sorted, unique candidates t
func sortUnique(values []ImageID) → []ImageID  :188-191
func intersectSortedSuffix(dst []ImageID, start int, right []ImageID) → []ImageID  :193-210
```

### search/plan.go
```
type Stage struct  :34-37  # Stage describes one compiled candidate stage
type stage struct  :42-55  # stage is the unified compiled unit
type Plan struct  :182-187  # Plan is a reusable, cardinality-ordered cross-index query
type timestampRange struct  :215-219  # timestampRange is a coalesced lossy-only range for one index
func (kind) String() → string  :20-31
func (s) metadata() → Stage  :57-59
func (s) canFilter() → bool  :67-78  # canFilter reports whether filter can verify candidates witho
func (s) collect(dst []ImageID) → []ImageID  :80-94
func (s) filter(dst, candidates, scratch []ImageID, ws *Workspace) → []ImageID  :96-116
func geoFilter(dst, candidates, scratch []ImageID, ws *Workspace, s *stage) → []ImageID  :129-143  # geoFilter restricts candidates to the stage's ranges
func smallestBitmap(indexes []*bitmapidx.Index) → *bitmapidx.Index  :145-154
func collectBitmaps(dst []ImageID, indexes []*bitmapidx.Index) → []ImageID  :156-169
func bitmapEstimate(indexes []*bitmapidx.Index) → uint64  :171-179
func (plan) IsEmpty() → bool  :190-192  # IsEmpty reports whether the plan is valid but matches nothin
func (plan) Len() → int  :195-197  # Len returns the number of compiled stages
func (plan) Estimate() → uint64  :201-212  # Estimate returns the smallest stage estimate, or 0 for empty
func Compile(clauses ...Clause) → (Plan, error)  :225-308  # Compile validates clauses, combines bitmap restrictions, coa
func MustCompile(clauses ...Clause) → Plan  :357-363  # MustCompile is like Compile but panics on error
func (plan) Stages() → []Stage  :366-372  # Stages returns a copy of plan metadata in execution order
func (plan) Explain() → []Stage  :375-377  # Explain returns human-readable stage metadata in execution o
```

### search/search_test.go
```
func TestCompileAndSearchCombinedQuery(t *testing.T)  :14-84
func TestWithinCellsAcceptsCellIDsAndCoarseCells(t *testing.T)  :94-123
func TestSearchUsesExactTimestampLookup(t *testing.T)  :125-152
func TestSearchRejectsEmptyAndNilIndexQueries(t *testing.T)  :154-164
func TestExactTimestampFiltersBitmapCandidates(t *testing.T)  :166-198
func TestBitmapOnlyCountMatchesSearch(t *testing.T)  :200-228
func TestGeoStageFiltersSmallCandidatesAndCollectsLarge(t *testing.T)  :230-300
```

### search/service_stress_test.go
```
type stressFixture struct  :29-36
type loadResult struct  :108-112  # loadResult aggregates one concurrent run
func buildStressFixture(tb testing.TB) → *stressFixture  :38-96
func checkSortedUnique(ids []ImageID) → bool  :98-105
func runLoad(fixture *stressFixture, readers, writers, iters, rounds, batchSize int) → (loadResult, error)  :117-188  # runLoad fires readers query agents and writers mutation agen
func logLoadResult(tb testing.TB, name string, result loadResult, writers int)  :214-227
func TestServiceConcurrentReadersWriters(t *testing.T)  :231-243  # TestServiceConcurrentReadersWriters runs 100 query agents ag
func TestServiceReaderSweep(t *testing.T)  :247-257  # TestServiceReaderSweep varies reader pressure at fixed write
func TestServiceGOGCComparison(t *testing.T)  :261-277  # TestServiceGOGCComparison reruns the load at GOGC=200 to iso
func BenchmarkServiceQuery(b *testing.B)  :279-287
func BenchmarkServiceQueryParallel(b *testing.B)  :289-299
func BenchmarkServiceWorkspace(b *testing.B)  :303-318  # BenchmarkServiceWorkspace isolates the pool: pooled workspac
func BenchmarkServiceDstSizing(b *testing.B)  :322-337  # BenchmarkServiceDstSizing isolates caller-buffer reuse: a pr
func BenchmarkServiceStaleVsFreshPlan(b *testing.B)  :341-376  # BenchmarkServiceStaleVsFreshPlan grows the indexes the way w
```

### search/service.go
```
type Service struct  :23-30  # Service is a concurrency-safe front door for one compiled Pl
func NewService(plan Plan, workspaceCap, batchCap int) → *Service  :35-47  # NewService returns a Service sharing plan across agents
func (service) Estimate() → uint64  :50-52  # Estimate returns the shared plan's smallest stage estimate
func (service) Prewarm(n int)  :57-61  # Prewarm places n ready workspaces into the pool so the first
func (service) IsEmpty() → bool  :64-66  # IsEmpty reports whether the shared plan matches nothing
func (service) Query(dst []ImageID) → []ImageID  :70-73  # Query appends the plan's sorted, unique intersection to dst
func (service) QueryContext(ctx context.Context, dst []ImageID) → ([]ImageID, error)  :76-85  # QueryContext is Query with cancellation checked between stag
func (service) Count() → int  :88-97  # Count returns the number of matches without retaining them
func (service) AcquireBatch() → *timeidx.Batch  :101-107  # AcquireBatch checks out an exclusive timestamp batch sized f
func (service) ReleaseBatch(batch *timeidx.Batch)  :111-119  # ReleaseBatch returns a batch to the pool
```
