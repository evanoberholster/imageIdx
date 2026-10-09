package geoidx

import (
	"cmp"
	"errors"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

func testKeys(n int) []S2Key {
	rng := rand.New(rand.NewSource(5))
	keys := make([]S2Key, n)
	for i := range keys {
		key, err := KeyFromLatLng(testLat+(rng.Float64()*2-1)*testSpan, testLng+(rng.Float64()*2-1)*testSpan)
		if err != nil {
			panic(err)
		}
		keys[i] = key
	}
	return keys
}

func insertKeys(t testing.TB, index *Index, keys []S2Key, base uint64) {
	t.Helper()
	for i, key := range keys {
		if err := index.Insert(key, base+uint64(i)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
}

// TestBackgroundCompactionPreservesData drives many small writes through several
// delta rotations and checks the data survives a full drain.
func TestBackgroundCompactionPreservesData(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 64, MaxDelta: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	keys := testKeys(5000)
	insertKeys(t, index, keys, 1)
	if got := index.Len(); got != 5000 {
		t.Fatalf("Len = %d, want 5000", got)
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := index.Len(); got != 5000 {
		t.Fatalf("Len after Compact = %d", got)
	}
	// Every key must resolve to its image after compaction.
	for i := 0; i < 5000; i += 137 {
		if !index.Contains(keys[i], uint64(i+1)) {
			t.Fatalf("missing image %d after compaction", i+1)
		}
	}
}

// TestConcurrentReadersDuringRotation keeps lock-free readers running while the
// writer inserts (forcing rotations and compaction). Run with -race.
func TestConcurrentReadersDuringRotation(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 64, MaxDelta: 512})
	if err != nil {
		t.Fatal(err)
	}
	seed := testKeys(1000)
	insertKeys(t, index, seed, 1)
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
			m := roar.NewMutableBitmap()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = index.SearchRanges(nil, all)
				_, _ = index.Nearest(nil, testLat, testLng, 5)
				m.Reset()
				index.CollectRanges(m, all)
			}
		}()
	}
	more := testKeys(4000)
	insertKeys(t, index, more, 10_000)
	close(stop)
	wg.Wait()
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := index.Len(); got != 5000 {
		t.Fatalf("Len = %d, want 5000", got)
	}
}

// TestWritesAfterCompactionBegins proves mutations arriving while a compaction is
// mid-flight are retained and not folded into the snapshot.
func TestWritesAfterCompactionBegins(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	first := testKeys(100)
	insertKeys(t, index, first, 1)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	persistFail = func(stage persistStage) error {
		if stage == persistCreate {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	defer func() { persistFail = nil }()

	done := make(chan error, 1)
	go func() { done <- index.Compact() }()
	<-entered

	// Writer proceeds while compaction is paused.
	second := testKeys(50)
	insertKeys(t, index, second, 1000)
	if got := index.Len(); got != 150 {
		t.Fatalf("Len during compaction = %d, want 150", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := index.Len(); got != 150 {
		t.Fatalf("Len after compaction = %d, want 150", got)
	}
	for i := 0; i < 50; i++ {
		if !index.Contains(second[i], uint64(1000+i)) {
			t.Fatalf("post-snapshot image %d lost", 1000+i)
		}
	}
}

// TestCompactionFailureRetainsMutations checks a failed compaction keeps pending
// mutations and a later successful retry folds them.
func TestCompactionFailureRetainsMutations(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	keys := testKeys(200)
	insertKeys(t, index, keys, 1)

	persistFail = func(stage persistStage) error {
		if stage == persistCreate {
			return errors.New("boom")
		}
		return nil
	}
	if err := index.Compact(); err == nil {
		t.Fatal("Compact succeeded despite injected failure")
	}
	if got := index.Len(); got != 200 {
		t.Fatalf("Len after failed compaction = %d, want 200", got)
	}
	for i := 0; i < 200; i += 37 {
		if !index.Contains(keys[i], uint64(i+1)) {
			t.Fatalf("image %d lost by failed compaction", i+1)
		}
	}
	persistFail = nil
	if err := index.Compact(); err != nil {
		t.Fatalf("retry Compact: %v", err)
	}
	if got := index.Len(); got != 200 {
		t.Fatalf("Len after retry = %d, want 200", got)
	}
}

// TestCloseDuringCompaction checks Close waits for the background worker.
func TestCloseDuringCompaction(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21})
	if err != nil {
		t.Fatal(err)
	}
	insertKeys(t, index, testKeys(100), 1)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	persistFail = func(stage persistStage) error {
		if stage == persistCreate {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	defer func() { persistFail = nil }()
	go func() { _ = index.Compact() }()
	<-entered

	closed := make(chan struct{})
	go func() { _ = index.Close(); close(closed) }()
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind compaction")
	}
}

// TestRestartAfterIncompleteCheckpoint checks that only the last durable
// checkpoint survives a restart when writes were not checkpointed.
func TestRestartAfterIncompleteCheckpoint(t *testing.T) {
	dir := t.TempDir()
	config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
	index, err := New(dir, config)
	if err != nil {
		t.Fatal(err)
	}
	durable := testKeys(100)
	insertKeys(t, index, durable, 1)
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	insertKeys(t, index, testKeys(50), 1000) // not checkpointed
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, config)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Len(); got != 100 {
		t.Fatalf("reopened Len = %d, want 100", got)
	}
	for i := 0; i < 100; i += 11 {
		if !reopened.Contains(durable[i], uint64(i+1)) {
			t.Fatalf("durable image %d missing after reopen", i+1)
		}
	}
}

// TestMultiChunkCollection exercises the collector's chunk boundaries with a
// region returning far more than one chunk.
func TestMultiChunkCollection(t *testing.T) {
	index, entries := testEntries(t, 50_000, testSpan)
	ranges := testCover(5000)
	want := index.SearchRanges(nil, ranges)
	if len(want) <= collectChunk {
		t.Fatalf("test needs > %d matches, got %d", collectChunk, len(want))
	}
	m := roar.NewMutableBitmap()
	index.CollectRanges(m, ranges)
	if got := m.AppendTo(nil); !equalIDs(got, want) {
		t.Fatalf("multi-chunk collect mismatch: got %d, want %d", len(got), len(want))
	}
	candidates := make([]uint64, 0, len(want))
	for id := uint64(1); id <= uint64(len(entries)); id++ {
		candidates = append(candidates, id)
	}
	bit := roar.MustFromSorted(candidates)
	m.Reset()
	index.FilterRanges(m, bit, ranges)
	if got := m.AppendTo(nil); !equalIDs(got, want) {
		t.Fatalf("multi-chunk filter mismatch: got %d, want %d", len(got), len(want))
	}
}

// TestInsertDeleteMoveDeterminism locks down duplicate, stale-key, and move
// behavior within the effective view.
func TestInsertDeleteMoveDeterminism(t *testing.T) {
	index, err := NewMemory(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	keyA, _ := KeyFromLatLng(testLat, testLng)
	keyB, _ := KeyFromLatLng(testLat+0.01, testLng)

	if err := index.Insert(keyA, 1); err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(keyA, 1); err != nil { // duplicate insert
		t.Fatal(err)
	}
	if err := index.Insert(keyA, 2); err != nil {
		t.Fatal(err)
	}
	// Delete with a stale key still removes by ImageID.
	if err := index.Delete(keyB, 1); err != nil {
		t.Fatal(err)
	}
	if index.Contains(keyA, 1) {
		t.Fatal("image 1 not removed by stale-key delete")
	}
	if !index.Contains(keyA, 2) {
		t.Fatal("image 2 missing")
	}
	// Move image 2 to keyB.
	if err := index.Delete(keyA, 2); err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(keyB, 2); err != nil {
		t.Fatal(err)
	}
	if index.Contains(keyA, 2) || !index.Contains(keyB, 2) {
		t.Fatal("move not applied")
	}
	if got := index.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func equalIDs(a, b []uint64) bool {
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

// TestCompactionCommitReconcilesAfterRename simulates the manifest rename
// succeeding but the commit reporting an error (for example a directory-sync
// failure). The committed generation must be adopted, not discarded.
func TestCompactionCommitReconcilesAfterRename(t *testing.T) {
	dir := t.TempDir()
	config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
	index, err := New(dir, config)
	if err != nil {
		t.Fatal(err)
	}
	insertKeys(t, index, testKeys(100), 1)
	before := index.Generation()

	writeManifestFunc = func(dir string, gen, count uint64) error {
		if err := writeManifest(dir, gen, count); err != nil {
			return err
		}
		return errors.New("simulated post-rename sync failure")
	}
	defer func() { writeManifestFunc = writeManifest }()

	if err := index.Compact(); err != nil {
		t.Fatalf("Compact returned %v despite a committed manifest", err)
	}
	if index.Generation() == before {
		t.Fatal("generation not advanced after reconciled commit")
	}
	if got := index.Len(); got != 100 {
		t.Fatalf("Len = %d, want 100", got)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, config)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Len(); got != 100 {
		t.Fatalf("reopened Len = %d, want 100", got)
	}
}

// TestCloseRacesCommit blocks compaction immediately before the manifest commit,
// races Close, then releases the commit and requires Close to finish and the
// committed data to remain readable.
func TestCloseRacesCommit(t *testing.T) {
	dir := t.TempDir()
	config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
	index, err := New(dir, config)
	if err != nil {
		t.Fatal(err)
	}
	insertKeys(t, index, testKeys(100), 1)

	commitEntered := make(chan struct{})
	allowCommit := make(chan struct{})
	var once sync.Once
	persistFail = func(stage persistStage) error {
		if stage == persistCommit {
			once.Do(func() { close(commitEntered) })
			<-allowCommit
		}
		return nil
	}
	defer func() { persistFail = nil }()

	go func() { _ = index.Compact() }()
	<-commitEntered

	closed := make(chan struct{})
	go func() { _ = index.Close(); close(closed) }()
	close(allowCommit)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind commit")
	}

	reopened, err := Open(dir, config)
	if err != nil {
		t.Fatalf("reopen after close/commit race: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() < 100 {
		t.Fatalf("reopened Len = %d, want >= 100", reopened.Len())
	}
}

// TestSlowReaderDelaysReclamation holds a reader view while compaction publishes
// a new generation, then releases it and requires reclamation to complete. The
// reader touches the old base to prove it was not unmapped prematurely.
func TestSlowReaderDelaysReclamation(t *testing.T) {
	dir := t.TempDir()
	config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
	index, err := New(dir, config)
	if err != nil {
		t.Fatal(err)
	}
	insertKeys(t, index, testKeys(100), 1)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	insertKeys(t, index, testKeys(50), 1000)

	readerEntered := make(chan struct{})
	readerRelease := make(chan struct{})
	go func() {
		index.withView(func(v *view) {
			close(readerEntered)
			<-readerRelease
			if len(v.keys) > 0 {
				_ = v.keys[0]
				_ = v.ids[0]
			}
		})
	}()
	<-readerEntered

	reclaimEntered := make(chan struct{})
	allowReclaim := make(chan struct{})
	var once sync.Once
	persistFail = func(stage persistStage) error {
		if stage == persistReclaim {
			once.Do(func() { close(reclaimEntered) })
			<-allowReclaim
		}
		return nil
	}
	defer func() { persistFail = nil }()

	compacted := make(chan error, 1)
	go func() { compacted <- index.Compact() }()
	<-reclaimEntered

	// The new generation is published; reclamation is in progress and the reader
	// still holds the old view.
	index.mu.Lock()
	stillCompacting := index.compacting
	index.mu.Unlock()
	if !stillCompacting {
		t.Fatal("compaction finished before the reader released")
	}

	close(allowReclaim)
	close(readerRelease)
	select {
	case err := <-compacted:
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("compaction did not finish after the reader released")
	}
	if got := index.Len(); got != 150 {
		t.Fatalf("Len = %d, want 150", got)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
}

// checks that no committed data is lost, no mutation is applied twice, the index
// reopens, and Close completes.
func TestCompactionFailureStages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage persistStage
	}{
		{"create", persistCreate},
		{"write", persistWrite},
		{"sync", persistSync},
		{"commit", persistCommit},
		{"publish", persistPublish},
		{"reclaim", persistReclaim},
		{"remove", persistRemove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
			index, err := New(dir, config)
			if err != nil {
				t.Fatal(err)
			}
			keys := testKeys(300)
			insertKeys(t, index, keys, 1)
			if err := index.Checkpoint(); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			extra := testKeys(50)
			insertKeys(t, index, extra, 1000)

			persistFail = func(stage persistStage) error {
				if stage == tc.stage {
					return errors.New("injected")
				}
				return nil
			}
			_ = index.Compact()
			persistFail = nil

			if got := index.Len(); got != 350 {
				t.Fatalf("Len after %s failure = %d, want 350", tc.name, got)
			}
			for i := 0; i < 50; i++ {
				if !index.Contains(extra[i], uint64(1000+i)) {
					t.Fatalf("post-snapshot image %d lost after %s failure", 1000+i, tc.name)
				}
			}
			if err := index.Close(); err != nil {
				t.Fatalf("Close after %s failure: %v", tc.name, err)
			}
			reopened, err := Open(dir, config)
			if err != nil {
				t.Fatalf("Open after %s failure: %v", tc.name, err)
			}
			defer reopened.Close()
			if reopened.Len() < 300 {
				t.Fatalf("reopened Len = %d after %s failure, want >= 300", reopened.Len(), tc.name)
			}
			for i := 0; i < 300; i += 29 {
				if !reopened.Contains(keys[i], uint64(i+1)) {
					t.Fatalf("durable image %d lost after %s failure/reopen", i+1, tc.name)
				}
			}
		})
	}
}

// TestExactCountMatchesMerge drives randomized base/delta combinations, many with
// repeated keys, and requires the streaming exactCount to equal mergeInto's
// written count and a naive reference set. It locks down the invariant that
// sizing and merge output agree exactly.
func TestExactCountMatchesMerge(t *testing.T) {
	rng := rand.New(rand.NewSource(97))
	keyA, _ := KeyFromLatLng(testLat, testLng)
	keyB, _ := KeyFromLatLng(testLat+0.01, testLng)
	keyC, _ := KeyFromLatLng(testLat, testLng+0.01)
	keys := []S2Key{keyA, keyB, keyC}

	type pair struct {
		key S2Key
		id  common.ImageID
	}

	for round := 0; round < 1000; round++ {
		n := rng.Intn(300)
		base := make([]pair, n)
		for i := range base {
			base[i] = pair{key: keys[rng.Intn(len(keys))], id: common.ImageID(i + 1)}
		}
		slices.SortFunc(base, func(a, b pair) int {
			if order := cmp.Compare(a.key, b.key); order != 0 {
				return order
			}
			return cmp.Compare(a.id, b.id)
		})
		baseKeys := make([]S2Key, n)
		baseIDs := make([]common.ImageID, n)
		for i := range base {
			baseKeys[i] = base[i].key
			baseIDs[i] = base[i].id
		}

		// An id-unique delta: each named id is an insert, a delete, or untouched,
		// plus fresh inserts beyond the base.
		var frozen []mutation
		for id := common.ImageID(1); id <= common.ImageID(n); id++ {
			switch rng.Intn(3) {
			case 0:
				frozen = append(frozen, mutation{id: id, key: keys[rng.Intn(len(keys))], add: false})
			case 1:
				frozen = append(frozen, mutation{id: id, key: keys[rng.Intn(len(keys))], add: true})
			}
		}
		for k := 0; k < rng.Intn(20); k++ {
			frozen = append(frozen, mutation{id: common.ImageID(n + 1 + k), key: keys[rng.Intn(len(keys))], add: true})
		}
		slices.SortFunc(frozen, compareMutationID)

		shadow := newMutationSet(frozen, len(baseKeys))
		adds := sortedByKey(frozen)
		count := exactCount(baseKeys, baseIDs, shadow, adds)

		outKeys := make([]S2Key, len(baseKeys)+len(adds))
		outIDs := make([]common.ImageID, len(baseKeys)+len(adds))
		written := mergeInto(outKeys, outIDs, baseKeys, baseIDs, shadow, adds)
		if uint64(written) != count {
			t.Fatalf("round %d: exactCount=%d, mergeInto=%d", round, count, written)
		}

		ref := make(map[pair]bool)
		for i := range baseKeys {
			if !shadow.touched(baseIDs[i]) {
				ref[pair{key: baseKeys[i], id: baseIDs[i]}] = true
			}
		}
		for _, m := range adds {
			ref[pair{key: m.key, id: m.id}] = true
		}
		if uint64(len(ref)) != count {
			t.Fatalf("round %d: reference=%d, exactCount=%d", round, len(ref), count)
		}

		got := make(map[pair]bool, written)
		for i := 0; i < written; i++ {
			p := pair{key: outKeys[i], id: outIDs[i]}
			if got[p] {
				t.Fatalf("round %d: duplicate output pair (%d,%d)", round, p.key, p.id)
			}
			got[p] = true
		}
		if len(got) != len(ref) {
			t.Fatalf("round %d: output set=%d, reference=%d", round, len(got), len(ref))
		}
	}
}

// TestRepeatedKeyCompaction drives inserts, deletes, and moves among many images
// that all share one S2Key, then compacts and verifies the effective set. It
// covers the identical-key run handling in exactCount/mergeInto end to end.
func TestRepeatedKeyCompaction(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 8, CompactionTrigger: 16, MaxDelta: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	keyA, err := KeyFromLatLng(testLat, testLng)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := KeyFromLatLng(testLat+0.01, testLng)
	if err != nil {
		t.Fatal(err)
	}

	const n = 2000
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{Key: keyA, ImageID: common.ImageID(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		t.Fatal(err)
	}

	// Delete every fifth, move every third, insert n more at keyA.
	var inserts, deletes []Entry
	live := make(map[common.ImageID]S2Key, 2*n)
	for i := 0; i < n; i++ {
		live[common.ImageID(i+1)] = keyA
	}
	for i := 0; i < n; i++ {
		id := common.ImageID(i + 1)
		if i%5 == 0 {
			deletes = append(deletes, Entry{Key: keyA, ImageID: id})
		}
		if i%3 == 0 {
			inserts = append(inserts, Entry{Key: keyB, ImageID: id})
		}
	}
	for k := 0; k < n; k++ {
		inserts = append(inserts, Entry{Key: keyA, ImageID: common.ImageID(n + 1 + k)})
	}
	// Reference: Apply records inserts then deletes, so a later delete wins.
	for _, e := range inserts {
		live[e.ImageID] = e.Key
	}
	for _, e := range deletes {
		delete(live, e.ImageID)
	}

	if err := index.Apply(inserts, deletes); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}

	if got := index.Len(); got != uint64(len(live)) {
		t.Fatalf("Len = %d, want %d", got, len(live))
	}
	for id, key := range live {
		if !index.Contains(key, id) {
			t.Fatalf("image %d missing at its key %d", id, key)
		}
	}
}
