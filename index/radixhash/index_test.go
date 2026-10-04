package radixhash

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

const testArenaBytes = 16 << 20

func newTestIndex(t *testing.T, arenaBytes uint64, config Config) *Index {
	t.Helper()
	index, err := New(arenaBytes, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return index
}

// hashWithPrefix copies prefix and derives the remaining bytes from seed, so
// distinct seeds share the prefix but differ in their unconsumed tail.
func hashWithPrefix(prefix []byte, seed uint64) Hash {
	var h Hash
	copy(h[:], prefix)
	for i := len(prefix); i < hashBytes; i++ {
		h[i] = byte(seed >> (8 * uint((i-len(prefix))%8)))
	}
	return h
}

func TestLeafCapacityIsConsistent(t *testing.T) {
	classes := []uint32{512, 1024, 2048, 4096, 8192, 16384, 65536}
	for _, idBytes := range []int{id32Bytes, id64Bytes} {
		for depth := 1; depth <= maxDepth; depth++ {
			for _, class := range classes {
				capacity := capacityForSize(depth, idBytes, class)
				if capacity < 1 {
					t.Fatalf("capacityForSize(depth=%d, id=%d, %d) = %d", depth, idBytes, class, capacity)
				}
				if got := sizeForCapacity(depth, idBytes, capacity); got > int(class) {
					t.Fatalf("depth %d id %d capacity %d needs %d bytes, class %d", depth, idBytes, capacity, got, class)
				}
				if next := sizeForCapacity(depth, idBytes, capacity+1); next <= int(class) {
					t.Fatalf("depth %d id %d capacity %d also fits class %d", depth, idBytes, capacity+1, class)
				}
			}
		}
	}
}

func TestApplyLookupRoundTrip(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{})
	defer index.Close()

	rng := rand.New(rand.NewSource(1))
	const distinct = 500
	const total = 8000
	hashes := make([]Hash, distinct)
	for i := range hashes {
		hashes[i] = hashWithPrefix([]byte{byte(rng.Intn(16)), byte(rng.Intn(16))}, uint64(i)+1)
	}
	model := make(map[Hash][]ImageID)
	var batch []Entry
	for id := ImageID(1); id <= total; id++ {
		h := hashes[rng.Intn(len(hashes))]
		model[h] = append(model[h], id)
		batch = append(batch, Entry{Hash: h, ImageID: id})
		if len(batch) == 128 {
			if err := index.Apply(batch); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			batch = batch[:0]
		}
	}
	if err := index.Apply(batch); err != nil {
		t.Fatalf("Apply tail: %v", err)
	}

	var got []ImageID
	for h, want := range model {
		want = uniqueSorted(want)
		got = index.Lookup(got[:0], h)
		if !equalIDs(got, want) {
			t.Fatalf("Lookup(%x) = %v, want %v", h[:4], got, want)
		}
		if index.Count(h) != uint64(len(want)) {
			t.Fatalf("Count(%x) = %d, want %d", h[:4], index.Count(h), len(want))
		}
		if !index.Contains(h) {
			t.Fatalf("Contains(%x) = false", h[:4])
		}
	}
	missing := hashWithPrefix([]byte{200, 201}, 999)
	if got := index.Lookup(nil, missing); len(got) != 0 {
		t.Fatalf("Lookup(missing) = %v", got)
	}
	if meta := index.Metadata(); meta.Entries != total {
		t.Fatalf("Metadata.Entries = %d, want %d", meta.Entries, total)
	}
}

func TestDuplicatesAreAscendingAndDeduplicated(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{1, 2}, 7)
	if err := index.Apply([]Entry{
		{Hash: h, ImageID: 9},
		{Hash: h, ImageID: 3},
		{Hash: h, ImageID: 5},
		{Hash: h, ImageID: 3},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{3, 5, 9}) {
		t.Fatalf("Lookup = %v, want [3 5 9]", got)
	}
}

func TestSharedPrefixDifferentTail(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	a := hashWithPrefix([]byte{5, 5}, 100)
	b := hashWithPrefix([]byte{5, 5}, 200)
	if err := index.Apply([]Entry{{Hash: a, ImageID: 1}, {Hash: b, ImageID: 2}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := index.Lookup(nil, a); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("Lookup(a) = %v", got)
	}
	if got := index.Lookup(nil, b); !equalIDs(got, []ImageID{2}) {
		t.Fatalf("Lookup(b) = %v", got)
	}
}

func TestLookupFiltered(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{2, 3}, 4)
	var entries []Entry
	for id := ImageID(1); id <= 10; id++ {
		entries = append(entries, Entry{Hash: h, ImageID: id})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := index.LookupFiltered(nil, h, func(id ImageID) bool { return id%2 == 0 })
	if !equalIDs(got, []ImageID{2, 4, 6, 8, 10}) {
		t.Fatalf("LookupFiltered = %v", got)
	}
}

func TestBatchRewritesBucketOnce(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{8, 8}, 8)
	var first []Entry
	for id := ImageID(1); id <= 10; id++ {
		first = append(first, Entry{Hash: h, ImageID: id})
	}
	if err := index.Apply(first); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := index.ReclaimAll(); err != nil {
		t.Fatalf("ReclaimAll: %v", err)
	}
	var second []Entry
	for id := ImageID(11); id <= 60; id++ {
		second = append(second, Entry{Hash: h, ImageID: id})
	}
	if err := index.Apply(second); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	// One 50-entry batch targeting a single bucket must replace exactly one
	// leaf, one L1 node, and the root -- not one leaf per entry.
	if retired := index.Metadata().RetiredExtents; retired != 3 {
		t.Fatalf("retired after second batch = %d, want 3", retired)
	}
	if got := index.Count(h); got != 60 {
		t.Fatalf("Count = %d, want 60", got)
	}
}

func TestPromotionCreatesThirdLevel(t *testing.T) {
	// A small threshold forces the single Hash[0]/Hash[1] bucket to promote.
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 2048})
	defer index.Close()

	seed := func(from, to int) []Entry {
		var entries []Entry
		for i := from; i < to; i++ {
			entries = append(entries, Entry{Hash: hashWithPrefix([]byte{7, 9}, uint64(i)+1), ImageID: ImageID(i + 1)})
		}
		return entries
	}
	if err := index.Apply(seed(0, 10)); err != nil {
		t.Fatalf("small Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth != 2 {
		t.Fatalf("depth after small batch = %d, want 2", depth)
	}
	if err := index.Apply(seed(10, 600)); err != nil {
		t.Fatalf("promoting Apply: %v", err)
	}
	meta := index.Metadata()
	if meta.Depth != 3 {
		t.Fatalf("depth after promotion = %d, want 3", meta.Depth)
	}
	if meta.RootBuckets != 1 {
		t.Fatalf("RootBuckets = %d, want 1", meta.RootBuckets)
	}
	for i := 0; i < 600; i++ {
		h := hashWithPrefix([]byte{7, 9}, uint64(i)+1)
		if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{ImageID(i + 1)}) {
			t.Fatalf("Lookup(%d) = %v, want [%d]", i, got, i+1)
		}
	}
}

func TestPromotionPreservesDuplicates(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 1024})
	defer index.Close()
	var entries []Entry
	for i := 0; i < 400; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{1, 1}, 5), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth < 3 {
		t.Fatalf("depth = %d, want >= 3 for a single-hash run", depth)
	}
	if got := index.Count(hashWithPrefix([]byte{1, 1}, 5)); got != 400 {
		t.Fatalf("Count = %d, want 400", got)
	}
}

func TestReclaimFreesRetiredExtents(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{3, 3}, 3)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The root node replaced by the batch is retired at the superseded
	// generation. A reader still pinned there keeps it alive.
	if index.Metadata().RetiredExtents != 1 {
		t.Fatalf("retired = %d, want 1", index.Metadata().RetiredExtents)
	}
	generation := index.Generation() - 1
	if err := index.Reclaim(generation); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if index.Metadata().RetiredExtents != 1 {
		t.Fatalf("retired after early reclaim = %d, want 1", index.Metadata().RetiredExtents)
	}
	if err := index.Reclaim(generation + 1); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if index.Metadata().RetiredExtents != 0 {
		t.Fatalf("retired after reclaim = %d, want 0", index.Metadata().RetiredExtents)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("Lookup after reclaim = %v", got)
	}
}

func TestApplyRejectsReservedImageID(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	if err := index.Apply([]Entry{{Hash: hashWithPrefix([]byte{1, 1}, 1), ImageID: 0}}); err == nil {
		t.Fatalf("Apply(ImageID 0) = nil, want error")
	}
}

func TestClosedIndexRejectsWrites(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := index.Apply([]Entry{{Hash: hashWithPrefix([]byte{1, 1}, 1), ImageID: 1}}); err != ErrClosed {
		t.Fatalf("Apply after Close = %v, want ErrClosed", err)
	}
}

func TestCorruptLeafIsDetected(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{1, 1}, 1)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ref, _, ok := index.traverse(index.view.Load(), h)
	if !ok {
		t.Fatalf("traverse miss")
	}
	offset := extentOffset(ref)
	before := append([]byte(nil), index.arena[offset:offset+leafHeaderSize]...)

	// Corrupt magic.
	index.arena[offset] = 0
	if _, _, err := readLeafSlots(index.arena, ref); err == nil {
		t.Fatalf("readLeafSlots accepted a bad magic")
	}
	copy(index.arena[offset:offset+leafHeaderSize], before)

	// Corrupt depth.
	index.arena[offset+6] = 0
	if _, _, err := readLeafSlots(index.arena, ref); err == nil {
		t.Fatalf("readLeafSlots accepted a bad depth")
	}
	copy(index.arena[offset:offset+leafHeaderSize], before)

	// A reader against a corrupt leaf returns no results rather than panicking.
	index.arena[offset+6] = 0
	if got := index.Lookup(nil, h); len(got) != 0 {
		t.Fatalf("Lookup of corrupt leaf = %v, want empty", got)
	}
}

func TestApplyRollbackOnOutOfSpace(t *testing.T) {
	// Root node (1024) plus a few two-level buckets, then an oversized batch.
	index, err := New(16*512, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer index.Close()

	var seeded []Entry
	for b := byte(0); b < 3; b++ {
		seeded = append(seeded, Entry{Hash: hashWithPrefix([]byte{b, 1}, 1), ImageID: ImageID(b + 1)})
	}
	if err := index.Apply(seeded); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	before := index.Metadata()

	var overflowing []Entry
	for b := byte(20); b < 30; b++ {
		overflowing = append(overflowing, Entry{Hash: hashWithPrefix([]byte{b, 1}, 1), ImageID: ImageID(b)})
	}
	if err := index.Apply(overflowing); err == nil {
		t.Fatalf("Apply over capacity = nil, want out-of-space")
	}
	after := index.Metadata()
	if after.Entries != before.Entries {
		t.Fatalf("entries changed on failed Apply: %d -> %d", before.Entries, after.Entries)
	}
	if after.RetiredExtents != before.RetiredExtents {
		t.Fatalf("retired %d -> %d after rollback, want unchanged", before.RetiredExtents, after.RetiredExtents)
	}
	if after.FreeBytes != before.FreeBytes {
		t.Fatalf("free bytes %d -> %d after rollback, want unchanged", before.FreeBytes, after.FreeBytes)
	}
	for b := byte(0); b < 3; b++ {
		h := hashWithPrefix([]byte{b, 1}, 1)
		if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{ImageID(b + 1)}) {
			t.Fatalf("seeded bucket %d = %v", b, got)
		}
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	index := newTestIndex(t, 32<<20, Config{})
	defer index.Close()

	const numHashes = 32
	hashes := make([]Hash, numHashes)
	for i := range hashes {
		hashes[i] = hashWithPrefix([]byte{byte(i), byte(i >> 3)}, uint64(i)+1)
	}

	var errOnce sync.Once
	var runErr error
	fail := func(err error) { errOnce.Do(func() { runErr = err }) }

	var readers sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var dst []ImageID
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, h := range hashes {
					dst = index.Lookup(dst[:0], h)
					if !sort.SliceIsSorted(dst, func(a, b int) bool { return dst[a] < dst[b] }) {
						fail(fmt.Errorf("lookup returned unsorted IDs: %v", dst))
						return
					}
				}
			}
		}()
	}

	const writers = 2
	const perWriter = 2000
	var writerGroup sync.WaitGroup
	for w := 0; w < writers; w++ {
		writerGroup.Add(1)
		go func(writer int) {
			defer writerGroup.Done()
			batch := make([]Entry, 0, 64)
			for n := 0; n < perWriter/64; n++ {
				batch = batch[:0]
				for k := 0; k < 64; k++ {
					h := hashes[(writer*n+k)%numHashes]
					batch = append(batch, Entry{Hash: h, ImageID: ImageID(writer*1_000_000 + n*64 + k + 1)})
				}
				if err := index.Apply(batch); err != nil {
					fail(err)
					return
				}
			}
		}(w)
	}
	writerGroup.Wait()
	close(stop)
	readers.Wait()
	if runErr != nil {
		t.Fatalf("concurrent run: %v", runErr)
	}
	if err := index.ReclaimAll(); err != nil {
		t.Fatalf("ReclaimAll: %v", err)
	}
}

func BenchmarkLookupTwoLevelVersusThreeLevel(b *testing.B) {
	for _, size := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("two-level/%d", size), func(b *testing.B) {
			benchmarkLookupShape(b, size, Config{PromotionBytes: 1 << 30})
		})
		b.Run(fmt.Sprintf("three-level/%d", size), func(b *testing.B) {
			benchmarkLookupShape(b, size, Config{PromotionBytes: 8 << 10})
		})
	}
}

func benchmarkLookupShape(b *testing.B, size int, config Config) {
	// Concentrate hashes into a small number of Hash[0]/Hash[1] buckets so the
	// promotion threshold is exercised.
	index, err := New(64<<20, config)
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close()
	rng := rand.New(rand.NewSource(42))
	hashes := make([]Hash, size)
	batch := make([]Entry, 0, 4096)
	for i := range hashes {
		hashes[i] = hashWithPrefix([]byte{byte(i % 8), byte((i / 8) % 8)}, uint64(rng.Int63())+1)
		batch = append(batch, Entry{Hash: hashes[i], ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(batch); err != nil {
		b.Fatal(err)
	}
	b.Logf("depth=%d", index.Metadata().Depth)
	var dst []ImageID
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.Lookup(dst[:0], hashes[i%len(hashes)])
	}
	_ = dst
}

func BenchmarkApplyBatch(b *testing.B) {
	index, err := New(256<<20, Config{})
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close()
	rng := rand.New(rand.NewSource(3))
	batch := make([]Entry, 256)
	for i := range batch {
		batch[i] = Entry{Hash: hashWithPrefix([]byte{byte(rng.Intn(256)), byte(rng.Intn(256))}, uint64(i)+1), ImageID: ImageID(i + 1)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := index.Apply(batch); err != nil {
			b.Fatal(err)
		}
		if err := index.ReclaimAll(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestLargeImageIDsUse64BitLeaves(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{4, 4}, 4)
	big := ImageID(1) << 40
	if err := index.Apply([]Entry{{Hash: h, ImageID: big}, {Hash: h, ImageID: big + 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{big, big + 1}) {
		t.Fatalf("Lookup = %v, want [%d %d]", got, big, big+1)
	}
}

func TestMaxUint64ImageID(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{5, 5}, 5)
	ids := []ImageID{1, 1 << 32, ^ImageID(0) - 1, ^ImageID(0)}
	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, Entry{Hash: h, ImageID: id})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	sorted := append([]ImageID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// The leaf starts with small ids (32-bit width) and must widen to 64-bit
	// when the maximum id is added.
	if got := index.Lookup(nil, h); !equalIDs(got, sorted) {
		t.Fatalf("Lookup = %v, want %v", got, sorted)
	}
	// A subsequent update must keep the widened leaf lossless.
	if err := index.Apply([]Entry{{Hash: h, ImageID: ^ImageID(0) - 2}}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	sorted = append(sorted, ^ImageID(0)-2)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if got := index.Lookup(nil, h); !equalIDs(got, sorted) {
		t.Fatalf("Lookup after widen = %v, want %v", got, sorted)
	}
}

func TestAutoReclaimBoundsRetired(t *testing.T) {
	index := newTestIndex(t, 4<<20, Config{AutoReclaimBytes: 4096})
	defer index.Close()
	h := hashWithPrefix([]byte{6, 6}, 6)
	for id := ImageID(1); id <= 400; id++ {
		if err := index.Apply([]Entry{{Hash: h, ImageID: id}}); err != nil {
			t.Fatalf("Apply %d: %v", id, err)
		}
	}
	if retired := index.Metadata().RetiredExtents; retired > 8 {
		t.Fatalf("retired = %d, want bounded by auto-reclaim", retired)
	}
	if got := index.Count(h); got != 400 {
		t.Fatalf("Count = %d, want 400", got)
	}
}

func TestCompactPreservesEntriesAndReclaims(t *testing.T) {
	index := newTestIndex(t, 8<<20, Config{PromotionBytes: 4096})
	defer index.Close()
	rng := rand.New(rand.NewSource(9))
	model := make(map[Hash][]ImageID)
	var batch []Entry
	for id := ImageID(1); id <= 5000; id++ {
		h := hashWithPrefix([]byte{byte(rng.Intn(8)), byte(rng.Intn(8))}, uint64(id))
		model[h] = append(model[h], id)
		batch = append(batch, Entry{Hash: h, ImageID: id})
		if len(batch) == 200 {
			if err := index.Apply(batch); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			batch = batch[:0]
		}
	}
	if err := index.Apply(batch); err != nil {
		t.Fatalf("Apply tail: %v", err)
	}
	beforeEntries := index.Metadata().Entries
	beforeFree := index.Metadata().FreeBytes
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	meta := index.Metadata()
	if meta.Entries != beforeEntries {
		t.Fatalf("entries after compact = %d, want %d", meta.Entries, beforeEntries)
	}
	if meta.FreeBytes < beforeFree {
		t.Fatalf("free bytes after compact = %d, want >= %d", meta.FreeBytes, beforeFree)
	}
	for h, want := range model {
		if got := index.Lookup(nil, h); !equalIDs(got, uniqueSorted(want)) {
			t.Fatalf("Lookup(%x) after compact = %v, want %v", h[:4], got, uniqueSorted(want))
		}
	}
	if err := index.ReclaimAll(); err != nil {
		t.Fatalf("ReclaimAll: %v", err)
	}
}

func TestMmapArena(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arena.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	h := hashWithPrefix([]byte{9, 9}, 9)
	var entries []Entry
	for id := ImageID(1); id <= 100; id++ {
		entries = append(entries, Entry{Hash: h, ImageID: id})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := index.Count(h); got != 100 {
		t.Fatalf("Count = %d, want 100", got)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func equalIDs(a, b []ImageID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// uniqueSorted returns a sorted copy with duplicates removed.
func uniqueSorted(ids []ImageID) []ImageID {
	out := append([]ImageID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	unique := out[:0]
	for i, id := range out {
		if i > 0 && out[i-1] == id {
			continue
		}
		unique = append(unique, id)
	}
	return unique
}

func FuzzIndexModel(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{0, 0, 0, 0, 1, 0, 2, 0, 0, 3, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		index, err := New(testArenaBytes, Config{PromotionBytes: 4096})
		if err != nil {
			t.Skip()
		}
		defer index.Close()
		model := make(map[Hash][]ImageID)
		var batch []Entry
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			if err := index.Apply(batch); err != nil {
				return false
			}
			batch = batch[:0]
			return true
		}
		const maxOps = 2048
		for i := 0; i+2 < len(data) && i/3 < maxOps; i += 3 {
			h := hashWithPrefix([]byte{data[i], data[i+1]}, uint64(data[i+2])+1)
			id := ImageID(data[i+2]) + 1
			model[h] = append(model[h], id)
			batch = append(batch, Entry{Hash: h, ImageID: id})
			if len(batch) == 16 && !flush() {
				t.Skip()
			}
		}
		if !flush() {
			t.Skip()
		}
		for h, want := range model {
			got := index.Lookup(nil, h)
			if !equalIDs(got, uniqueSorted(want)) {
				t.Fatalf("Lookup(%x) = %v, want %v", h[:4], got, uniqueSorted(want))
			}
		}
	})
}
