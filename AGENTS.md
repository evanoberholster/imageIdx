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
`idset`, and `internal/indexutil` mechanisms; `search` may combine indexes, but
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
roar/and.go                                   +filterArrayByBitmap  +filterArrayNotInBitmap  ~filterArrayByBitmap  ~filterArrayNotInBitmap
roar/andnot.go                                ~andNotContainerInto  ~andNotWordsArray
roar/apply.go                                 ~applyInto
roar/builder.go                               ~NewBuilder
roar/compact.go                               +assemble  +writeHeaderKeyTable  ~assemble
roar/container_array.go                       +shouldGallop  +lowerBoundFrom  +intersectArrays  +unionArrays
roar/container_bitmap.go                      +extractWords  ~extractWords  ~bitmapContains
roar/delta_test.go                            +TestDeltaBuilderAddRemove  +TestDeltaBuilderEmptyStart  +TestDeltaBuilderRemovalWinsOverAdd  +TestDeltaBuilderSetOps
roar/delta.go                                 +NewDeltaBuilder  +NewDeltaBuilderFrom  +bitmapFromUnique
roar/internal/bitmapkernel/avx2_amd64.go      +andAVX2FixedAsm  +orAVX2FixedAsm  +andNotAVX2FixedAsm  +applyAVX2FixedAsm
roar/internal/bitmapkernel/benchmark_test.go  +benchContainers  +BenchmarkAnd  +BenchmarkOr  +BenchmarkAndNot
roar/internal/bitmapkernel/correctness_test.go +refAnd  +refOr  +refAndNot  +refIntersects
roar/internal/bitmapkernel/kernel.go          +And  +Or  +AndNot  +Intersects
roar/internal/bitmapkernel/neon_arm64.go      +andNEONFixedAsm  +orNEONFixedAsm  +andNotNEONFixedAsm  +applyNEONFixedAsm
roar/internal/bitmapkernel/popcount_benchmark_avx2_test.go +BenchmarkPopcountScalarAVX2  +BenchmarkAndAVX2Go  +BenchmarkPopcountAVX2Asm
roar/internal/bitmapkernel/popcount_benchmark_simd_test.go +BenchmarkPopcountScalar  +BenchmarkPopcountNEON  +BenchmarkPopcountNEONAsm
roar/internal/bitmapkernel/scalar.go          +andScalar  +orScalar  +andNotScalar  +intersectsScalar
roar/internal/bitmapkernel/simd_test.go       +TestSIMDMatchesScalar  +TestSIMDTails
roar/keys.go                                  ~findKey
roar/many.go                                  ~andManyContainerInto
roar/mutable_bitmap.go                        +NewMutableBitmap  +NewMutableBitmapFrom
roar/or.go                                    ~orContainerInto
search/clause.go                              ~ExactTimestamp
search/execute.go                             +truncateLimit  ~Search
search/plan.go                                +smallestBitmap  +collectBitmaps  +bitmapEstimate  +MustCompile
```

## roar

### roar/and.go
```
func AndInto(dst *Builder, a, b Bitmap)  :9-24  # AndInto appends the intersection of a and b to dst
func andContainerInto(dst *Builder, key uint64, a, b []byte)  :26-42
func filterArrayByBitmap(dst *[]uint16, values []uint16, words []uint64)  :45-53  # filterArrayByBitmap writes the values present in words to *d
func filterArrayNotInBitmap(dst *[]uint16, values []uint16, words []uint64)  :56-64  # filterArrayNotInBitmap writes the values absent from words t
func andWords(dst, a, b []uint64) → uint32  :68-70  # andWords writes the wordwise intersection into dst, which mu
func copyContainerInto(dst *Builder, key uint64, container []byte)  :73-85  # copyContainerInto appends an exact copy of a container to ds
func copyAllInto(dst *Builder, src Bitmap)  :88-92  # copyAllInto appends a complete copy of src to dst
```

### roar/andnot.go
```
func AndNotInto(dst *Builder, a, b Bitmap)  :6-23  # AndNotInto appends a minus b to dst
func andNotContainerInto(dst *Builder, key uint64, a, b []byte)  :25-41
func andNotWordsArray(dst, words []uint64, values []uint16) → uint32  :45-51  # andNotWordsArray writes a bitmap container minus a sparse ar
func andNotWords(dst, a, b []uint64) → uint32  :55-57  # andNotWords writes the wordwise difference into dst and retu
```

### roar/apply.go
```
func applyInto(dst *Builder, base, add, remove Bitmap)  :8-54  # applyInto appends (base ∪ add) ∖ remove to dst in one pass o
func combineThreeInto(dst *Builder, key uint64, base, add, remove []byte)  :60-91  # combineThreeInto appends (base ∪ add) ∖ remove to dst for on
func setContainerBits(words []uint64, container []byte)  :94-106  # setContainerBits ORs the set bits of a container into words
```

### roar/builder.go
```
type Builder struct  :25-34  # Builder accumulates result containers in key order for mater
func NewBuilder(capacity int) → *Builder  :37-47  # NewBuilder returns a Builder with room for capacity containe
func (b) wordsScratch() → []uint64  :50-55  # wordsScratch returns the dense word buffer, allocating it on
func (b) Reset()  :59-65  # Reset clears accumulated containers while retaining allocate
func (b) KeyCount() → int  :68-68  # KeyCount returns the number of containers accumulated so far
func (b) reserveData(n int)  :73-77  # reserveData ensures the container region can hold at least n
func (b) reserveKeys(n int)  :81-86  # reserveKeys ensures the key and offset slices can hold at le
func (b) prepareResult(keyCount, dataBytes int)  :91-95  # prepareResult readies the Builder for one result with at mos
func (b) extend(size int) → []byte  :99-104  # extend grows the container region by size zeroed bytes and r
func (b) appendArray(key uint64, values []uint16)  :106-120
func (b) appendBitmap(key uint64, words []uint64, card uint32)  :122-135
func (b) emitWords(key uint64, card uint32)  :140-150  # emitWords selects an array or bitmap container for a dense r
func (b) emitValues(key uint64, values []uint16)  :154-169  # emitValues selects an array or bitmap container for a sparse
func writeContainerHeader(dst []byte, blocks uint16, containerType ContainerType, card uint32)  :171-175
```

### roar/compact.go
```
func assemble(keys []uint64, offsets []uint64, data []byte, cardinality uint64) → []byte  :8-14  # assemble writes the header, key table, and container payload
func writeHeaderKeyTable(out []byte, keys, offsets []uint64, cardinality uint64)  :19-32  # writeHeaderKeyTable writes the header and key table into the
func (b) Compact() → []byte  :37-39  # Compact assembles the accumulated containers into a compact 
func (b) CompactInto(reuse []byte) → []byte  :44-55  # CompactInto assembles the accumulated containers into reuse'
func (b) finishResult(reuse []byte, owned bool) → []byte  :60-65  # finishResult returns the accumulated result
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

### roar/container_bitmap.go
```
func bitmapContains(words []uint64, x uint16) → bool  :10-12  # bitmapContains reports whether a bitmap container contains v
func andWordsCardinality(a, b []uint64) → uint64  :16-18  # andWordsCardinality counts the intersection of two bitmap co
func orWordsCardinality(a, b []uint64) → uint64  :22-24  # orWordsCardinality counts the union of two bitmap containers
func andNotWordsCardinality(a, b []uint64) → uint64  :28-30  # andNotWordsCardinality counts a minus b for two bitmap conta
func wordsIntersect(a, b []uint64) → bool  :33-35  # wordsIntersect reports whether two bitmap containers share a
func wordsCardinality(words []uint64) → uint64  :38-40  # wordsCardinality sums the set bits in a bitmap container
func extractWords(dst *[]uint16, words []uint64)  :44-54  # extractWords writes the set values of a bitmap container int
```

### roar/contains.go
```
func (v) Contains(id uint64) → bool  :6-22  # Contains reports whether id is present
```

### roar/delta_test.go
```
func TestDeltaBuilderAddRemove(t *testing.T)  :9-24
func TestDeltaBuilderEmptyStart(t *testing.T)  :26-34
func TestDeltaBuilderRemovalWinsOverAdd(t *testing.T)  :36-44
func TestDeltaBuilderSetOps(t *testing.T)  :46-67
func TestDeltaBuilderFlushBeforeSetOp(t *testing.T)  :69-79
func TestDeltaBuilderReset(t *testing.T)  :81-92
func TestDeltaBuilderFreezeReuseKeepsReturnedBitmapIndependent(t *testing.T)  :94-111
func TestDeltaBuilderIgnoresZero(t *testing.T)  :113-120
func TestDeltaBuilderMatchesReference(t *testing.T)  :122-146
func TestDeltaBuilderInlineBuffersAvoidAllocation(t *testing.T)  :148-164
```

### roar/delta.go
```
type DeltaBuilder struct  :34-42  # DeltaBuilder is a writer-owned builder that accumulates raw 
func NewDeltaBuilder() → *DeltaBuilder  :45-51  # NewDeltaBuilder returns an empty DeltaBuilder
func NewDeltaBuilderFrom(b Bitmap) → *DeltaBuilder  :55-61  # NewDeltaBuilderFrom returns a DeltaBuilder seeded with b
func (m) AddMany(ids []uint64)  :65-71  # AddMany stages additions of ids
func (m) Remove(id uint64)  :74-80  # Remove stages a removal of id
func (m) Or(b Bitmap)  :83-85  # Or unions b into the builder's current bitmap
func (m) And(b Bitmap)  :88-90  # And intersects b with the builder's current bitmap
func (m) AndNot(b Bitmap)  :93-95  # AndNot subtracts b from the builder's current bitmap
func (m) ApplyDelta(add, remove Bitmap)  :100-108  # ApplyDelta folds additions minus removals into the builder i
func (m) foldSet(operand Bitmap, keyCount, dataBytes int, op func(dst *Builder, a, b Bitmap) → )  :113-118  # foldSet applies one whole-set operation with the shared resu
func (m) Reset()  :122-128  # Reset clears the builder and returns its pending slices to t
func (m) Freeze() → Bitmap  :133-136  # Freeze materializes pending additions/removals and returns t
func (m) flush()  :140-155  # flush merges pending additions (then removals) into the curr
func bitmapFromUnique(ids []uint64) → Bitmap  :159-174  # bitmapFromUnique sorts ids in place, removes duplicates and 
```

### roar/filter_sorted.go
```
func (b) FilterSorted(dst, candidates []uint64) → []uint64  :9-23  # FilterSorted appends the candidates present in b to dst
```

### roar/internal/bitmapkernel/avx2_amd64.go
```
func andAVX2FixedAsm(dst, a, b []uint64) → uint32 func orAVX2FixedAsm(dst  :11-42
func andAVX2(dst, a, b []uint64) → uint32  :44-61
func orAVX2(dst, a, b []uint64) → uint32  :63-80
func andNotAVX2(dst, a, b []uint64) → uint32  :82-99
func intersectsAVX2(a, b []uint64) → bool  :101-118
func andCardinalityAVX2(a, b []uint64) → uint64  :120-135
func orCardinalityAVX2(a, b []uint64) → uint64  :137-152
func andNotCardinalityAVX2(a, b []uint64) → uint64  :154-169
func cardinalityAVX2(words []uint64) → uint64  :171-186
func andManyAVX2(dst []uint64, inputs ...[]uint64) → uint32  :188-219
func andMany5AVX2(dst, a, b, c, d, e []uint64) → uint32  :221-239
func andMany6AVX2(dst, a, b, c, d, e, f []uint64) → uint32  :241-260
func andMany7AVX2(dst, a, b, c, d, e, f, g []uint64) → uint32  :262-282
func andMany8AVX2(dst, a, b, c, d, e, f, g, h []uint64) → uint32  :284-305
func andMany3AVX2(dst, a, b, c []uint64) → uint32  :307-324
func andMany4AVX2(dst, a, b, c, d []uint64) → uint32  :326-343
func applyAVX2(dst, base, add, remove []uint64) → uint32  :345-364
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
func BenchmarkAndManyVariadic4(b *testing.B)  :144-155
func BenchmarkAndManyVariadic3(b *testing.B)  :157-164
func BenchmarkAndManyVariadic5To8(b *testing.B)  :166-184
func BenchmarkApply(b *testing.B)  :186-201
func BenchmarkScalarBaseline(b *testing.B)  :205-212  # BenchmarkScalarBaseline measures the scalar backend directly
```

### roar/internal/bitmapkernel/correctness_test.go
```
func refAnd(dst, a, b []uint64) → uint32  :13-21
func refOr(dst, a, b []uint64) → uint32  :23-31
func refAndNot(dst, a, b []uint64) → uint32  :33-41
func refIntersects(a, b []uint64) → bool  :43-50
func makeShape(shape string, rng *rand.Rand) → []uint64  :55-86  # makeShape returns a test container matching the named shape
func TestKernelsAgainstReference(t *testing.T)  :88-141
func TestFusedKernelsAgainstReference(t *testing.T)  :143-174
func TestGenericAndManyAgainstReference(t *testing.T)  :176-196
func refOrInto(a, b []uint64) → []uint64  :198-202
func refAndInto(a, b []uint64) → []uint64  :204-208
```

### roar/internal/bitmapkernel/kernel.go
```
func And(dst, a, b []uint64) → uint32  :27-27  # And writes a & b to dst and returns the exact cardinality of
func Or(dst, a, b []uint64) → uint32  :30-30  # Or writes a | b to dst and returns the exact cardinality of 
func AndNot(dst, a, b []uint64) → uint32  :33-33  # AndNot writes a &^ b to dst and returns the exact cardinalit
func Intersects(a, b []uint64) → bool  :36-36  # Intersects reports whether a and b share any set bit, exitin
func AndCardinality(a, b []uint64) → uint64  :39-39  # AndCardinality returns the cardinality of a & b without mate
func OrCardinality(a, b []uint64) → uint64  :42-42  # OrCardinality returns the cardinality of a | b without mater
func AndNotCardinality(a, b []uint64) → uint64  :46-46  # AndNotCardinality returns the cardinality of a &^ b without 
func Cardinality(w []uint64) → uint64  :49-49  # Cardinality returns the number of set bits in w
func AndMany(dst []uint64, inputs ...[]uint64) → uint32  :53-63  # AndMany writes the N-way intersection of inputs to dst and r
func AndMany3(dst, a, b, c []uint64) → uint32  :67-69  # AndMany3 writes a & b & c to dst without constructing a vari
func AndMany4(dst, a, b, c, d []uint64) → uint32  :73-75  # AndMany4 writes a & b & c & d to dst without constructing a 
func Apply(dst, base, add, remove []uint64) → uint32  :78-80  # Apply writes (base | add) &^ remove to dst and returns its c
```

### roar/internal/bitmapkernel/neon_arm64.go
```
func andNEONFixedAsm(dst, a, b []uint64) → uint32 func orNEONFixedAsm(dst  :11-42
func popcountBytesNEON(v archsimd.Uint64x2) → archsimd.Uint8x16  :46-48  # popcountBytesNEON returns one byte count for every byte in v
func reducePopcountNEON(counts archsimd.Uint8x16) → uint32  :53-57  # reducePopcountNEON horizontally sums sixteen byte counts wit
func countNEON(v0, v1, v2, v3 archsimd.Uint64x2) → uint32  :59-65
func countOneNEON(v archsimd.Uint64x2) → uint32  :67-69
func andNEON(dst, a, b []uint64) → uint32  :71-76
func orNEON(dst, a, b []uint64) → uint32  :78-83
func andNotNEON(dst, a, b []uint64) → uint32  :85-90
func intersectsNEON(a, b []uint64) → bool  :92-97
func andCardinalityNEON(a, b []uint64) → uint64  :99-104
func orCardinalityNEON(a, b []uint64) → uint64  :106-111
func andNotCardinalityNEON(a, b []uint64) → uint64  :113-118
func cardinalityNEON(words []uint64) → uint64  :120-125
func andManyNEON(dst []uint64, inputs ...[]uint64) → uint32  :127-181
func andMany3NEON(dst, a, b, c []uint64) → uint32  :183-206
func andMany4NEON(dst, a, b, c, d []uint64) → uint32  :208-231
func applyNEON(dst, base, add, remove []uint64) → uint32  :233-238
```

### roar/internal/bitmapkernel/popcount_benchmark_avx2_test.go
```
func BenchmarkPopcountScalarAVX2(b *testing.B)  :15-29
func BenchmarkAndAVX2Go(b *testing.B)  :31-49
func BenchmarkPopcountAVX2Asm(b *testing.B)  :51-61
```

### roar/internal/bitmapkernel/popcount_benchmark_simd_test.go
```
func BenchmarkPopcountScalar(b *testing.B)  :15-29
func BenchmarkPopcountNEON(b *testing.B)  :31-45
func BenchmarkPopcountNEONAsm(b *testing.B)  :47-57
```

### roar/internal/bitmapkernel/scalar.go
```
func andScalar(dst, a, b []uint64) → uint32  :14-22
func orScalar(dst, a, b []uint64) → uint32  :24-32
func andNotScalar(dst, a, b []uint64) → uint32  :34-42
func intersectsScalar(a, b []uint64) → bool  :44-51
func andCardinalityScalar(a, b []uint64) → uint64  :53-59
func orCardinalityScalar(a, b []uint64) → uint64  :61-67
func andNotCardinalityScalar(a, b []uint64) → uint64  :69-75
func cardinalityScalar(w []uint64) → uint64  :77-83
func andManyScalar(dst []uint64, inputs ...[]uint64) → uint32  :85-93
func andMany3Scalar(dst, a, b, c []uint64) → uint32  :95-103
func andMany4Scalar(dst, a, b, c, d []uint64) → uint32  :105-113
func applyScalar(dst, base, add, remove []uint64) → uint32  :115-123
```

### roar/internal/bitmapkernel/simd_test.go
```
func TestSIMDMatchesScalar(t *testing.T)  :14-54  # TestSIMDMatchesScalar compares the active SIMD backend (sele
func TestSIMDTails(t *testing.T)  :56-96
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

### roar/many.go
```
func AndManyInto(dst *Builder, views ...Bitmap)  :16-55  # AndManyInto appends the N-way intersection of views to dst w
func minEntryKey(entries [][]keyEntry, positions []int) → (uint64, bool)  :57-70
func andManyContainerInto(dst *Builder, key uint64, containers [][]byte)  :72-143
func valuePresentInAll(value uint16, driver int, containers [][]byte) → bool  :145-155
func AndManyCardinality(views ...Bitmap) → uint64  :159-210  # AndManyCardinality counts the N-way intersection of views wi
func andManyContainerCardinality(containers [][]byte) → uint64  :212-260
```

### roar/mutable_bitmap.go
```
type MutableBitmap struct  :17-24  # MutableBitmap is a single-owner roaring bitmap that shares t
func NewMutableBitmap() → *MutableBitmap  :27-31  # NewMutableBitmap returns an empty MutableBitmap
func NewMutableBitmapFrom(b Bitmap) → *MutableBitmap  :35-39  # NewMutableBitmapFrom returns a MutableBitmap seeded with b
func (m) FromSortedList(ids []uint64) → error  :44-52  # FromSortedList replaces the bitmap's content with a strictly
func (m) Or(b Bitmap)  :56-68  # Or unions b into the bitmap in place
func (m) And(b Bitmap)  :72-80  # And intersects b with the bitmap in place
func (m) AndNot(b Bitmap)  :83-90  # AndNot subtracts b from the bitmap in place
func (m) commit()  :94-97  # commit materializes the builder's result into m
func (m) Freeze() → Bitmap  :100-102  # Freeze returns the current bitmap as an immutable Bitmap wit
```

### roar/or.go
```
func OrInto(dst *Builder, a, b Bitmap)  :6-23  # OrInto appends the union of a and b to dst
func orContainerInto(dst *Builder, key uint64, a, b []byte)  :25-39
func orArrayBitmapInto(dst *Builder, key uint64, values []uint16, words []uint64, baseCard uint32)  :44-57  # orArrayBitmapInto unions a sparse array with a dense contain
func orWords(dst, a, b []uint64) → uint32  :60-62  # orWords writes the wordwise union into dst and returns its c
```

## search

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
func (plan) searchInto(ctx context.Context, dst []ImageID, workspace *Workspace, limit int) → ([]ImageID, error)  :96-166
func truncateLimit(dst []ImageID, start int, limit int) → []ImageID  :168-173
func sortUnique(values []ImageID) → []ImageID  :175-178
func intersectSortedSuffix(dst []ImageID, start int, right []ImageID) → []ImageID  :180-197
```

### search/plan.go
```
type Stage struct  :34-37  # Stage describes one compiled candidate stage
type stage struct  :42-55  # stage is the unified compiled unit
type Plan struct  :183-188  # Plan is a reusable, cardinality-ordered cross-index query
type timestampRange struct  :216-220  # timestampRange is a coalesced lossy-only range for one index
func (kind) String() → string  :20-31
func (s) metadata() → Stage  :57-59
func (s) canFilter() → bool  :67-78  # canFilter reports whether filter can verify candidates witho
func (s) collect(dst []ImageID) → []ImageID  :80-94
func (s) filter(dst, candidates, scratch []ImageID) → []ImageID  :96-116
func geoFilter(dst, candidates, scratch []ImageID, s *stage) → []ImageID  :129-144  # geoFilter restricts candidates to the stage's ranges
func smallestBitmap(indexes []*bitmapidx.Index) → *bitmapidx.Index  :146-155
func collectBitmaps(dst []ImageID, indexes []*bitmapidx.Index) → []ImageID  :157-170
func bitmapEstimate(indexes []*bitmapidx.Index) → uint64  :172-180
func (plan) IsEmpty() → bool  :191-193  # IsEmpty reports whether the plan is valid but matches nothin
func (plan) Len() → int  :196-198  # Len returns the number of compiled stages
func (plan) Estimate() → uint64  :202-213  # Estimate returns the smallest stage estimate, or 0 for empty
func Compile(clauses ...Clause) → (Plan, error)  :226-309  # Compile validates clauses, combines bitmap restrictions, coa
func MustCompile(clauses ...Clause) → Plan  :358-364  # MustCompile is like Compile but panics on error
func (plan) Stages() → []Stage  :367-373  # Stages returns a copy of plan metadata in execution order
func (plan) Explain() → []Stage  :376-378  # Explain returns human-readable stage metadata in execution o
```

### search/workspace.go
```
type Workspace struct  :9-11  # Workspace retains intermediate candidates between searches
func NewWorkspace(capacity int) → *Workspace  :13-18
func (workspace) Reset()  :21-25  # Reset clears temporary results while retaining allocated buf
func (workspace) Len() → int  :28-33  # Len returns the number of buffered intermediate IDs
func (workspace) Cap() → int  :36-41  # Cap returns the scratch buffer capacity
func (workspace) EnsureCapacity(n int)  :45-52  # EnsureCapacity grows scratch to hold at least n IDs without 
```
