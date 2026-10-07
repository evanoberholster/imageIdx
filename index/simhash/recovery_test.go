package simhash

import (
	"errors"
	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func journalSize(dir string) (int64, error) {
	fi, err := os.Stat(filepath.Join(dir, journalFilename))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// TestCheckpointMetadataBeforeManifest simulates a crash after shard metadata is
// written and flushed but before the manifest is published. The manifest stays
// authoritative (the newer shard slot is ignored) and WAL replay completes the
// update.
func TestCheckpointMetadataBeforeManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(100, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	before := st.Generation()
	target := randRecords(101, 1)[0]
	if err := st.Upsert(2, target); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Crash after metadata write+flush, before manifest publication.
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
		if err := sh.mapped.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
	if reopened.Generation() != before+1 {
		t.Fatalf("generation = %d, want %d", reopened.Generation(), before+1)
	}
}

// TestCheckpointManifestBeforeWALReset simulates a crash after the manifest is
// published but before the WAL is truncated. Replay must skip the already
// committed batch (idempotent) and keep the manifest generation.
func TestCheckpointManifestBeforeWALReset(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(102, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	before := st.Generation()
	target := randRecords(103, 1)[0]
	if err := st.Upsert(2, target); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
		if err := sh.mapped.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if err := st.writeManifestLocked(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := st.Close(); err != nil { // WAL not reset
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
	if reopened.Generation() != before+1 {
		t.Fatalf("generation = %d, want %d", reopened.Generation(), before+1)
	}
}

func TestCheckpointResetsWAL(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(104, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Upsert(2, randRecords(105, 1)[0]); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, journalFilename))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("WAL size after checkpoint = %d, want 0", fi.Size())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRecoveryIdempotent reopens twice and checkpoints between, asserting the
// same visible state.
func TestRecoveryIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(106, 16)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	target := randRecords(107, 1)[0]
	if err := st.UpsertBatch([]Update{{ImageID: 2, Record: target}, {ImageID: 9, Record: randRecords(108, 1)[0]}}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if err := st.Close(); err != nil { // no checkpoint: WAL replays on open
		t.Fatalf("Close: %v", err)
	}
	first, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen1: %v", err)
	}
	wantGen := first.Generation()
	want2, _ := first.Get(2)
	if err := first.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}
	second, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen2: %v", err)
	}
	defer second.Close()
	if second.Generation() != wantGen {
		t.Fatalf("generation drifted: %d != %d", second.Generation(), wantGen)
	}
	if got, _ := second.Get(2); got != want2 || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
}

func buildCheckpointed(t *testing.T, dir string, cfg Config, n int) *Store {
	t.Helper()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(int64(n), n)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return st
}

// A shard superblock newer than the manifest generation is an interrupted
// commit: open must ignore it and select the committed slot.
func TestShardNewerThanManifestSkipped(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	sh := st.shards[0]
	sh.generation = st.generation + 1
	if err := sh.writeMeta(); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 100 || reopened.Generation() != st.generation {
		t.Fatalf("reopened len/gen = %d/%d, want 100/%d", reopened.Len(), reopened.Generation(), st.generation)
	}
}

// If every shard slot is newer than the manifest, no committed metadata exists.
func TestShardNewerThanManifestBothSlotsCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	sh := st.shards[0]
	for i := 0; i < 2; i++ {
		sh.generation = st.generation + 1
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted shards newer than the manifest")
	}
}

// A manifest that claims more committed records than the shard contains is
// inconsistent.
func TestManifestNewerThanShardCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	gen := st.generation
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data := encodeManifest(manifest{
		pagesPerShard:   st.pagesPerShard,
		seqBlockRecords: uint64(defaultSeqBlockRecords),
		committed:       100_000,
		generation:      gen + 1,
		shardCount:      1,
	})
	if err := storage.WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a manifest newer than shard data")
	}
}

// A single corrupt superblock slot must not prevent opening from the other.
func TestCorruptShardSuperblockSlot(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := shardPath(dir, 0)
	file, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := file.WriteAt(make([]byte, storage.SuperblockSlotBytes), 0); err != nil {
		t.Fatalf("zero slot: %v", err)
	}
	_ = file.Close()
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 100 {
		t.Fatalf("reopened Len = %d, want 100", reopened.Len())
	}
}

func writeManifestForTest(t *testing.T, dir string, mf manifest) {
	t.Helper()
	if err := storage.WriteFileAtomic(dir, "manifest.tmp", "manifest", encodeManifest(mf), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestCorruptManifestGeometry(t *testing.T) {
	cfg := testConfig()
	valid := manifest{pagesPerShard: 1, seqBlockRecords: uint64(defaultSeqBlockRecords)}
	cases := []struct {
		name string
		mf   manifest
	}{
		{"pages-zero", manifest{pagesPerShard: 0, seqBlockRecords: uint64(defaultSeqBlockRecords)}},
		{"bad-seq", manifest{pagesPerShard: 1, seqBlockRecords: 1000}},
		{"committed-exceeds-capacity", manifest{pagesPerShard: 1, seqBlockRecords: uint64(defaultSeqBlockRecords), committed: 1 << 40, shardCount: 0}},
		{"shard-count-zero-with-committed", manifest{pagesPerShard: 1, seqBlockRecords: uint64(defaultSeqBlockRecords), committed: 5, shardCount: 0}},
		{"shard-count-too-big", manifest{pagesPerShard: 1, seqBlockRecords: uint64(defaultSeqBlockRecords), committed: 0, shardCount: ^uint64(0)}},
		{"size-overflow", manifest{pagesPerShard: 1 << 40, seqBlockRecords: uint64(defaultSeqBlockRecords)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestForTest(t, dir, tc.mf)
			if _, err := Open(dir, cfg); err == nil {
				t.Fatalf("Open accepted manifest %+v", tc.mf)
			}
		})
	}
	// A valid manifest passes validation.
	dir := t.TempDir()
	writeManifestForTest(t, dir, valid)
	if _, err := Open(dir, cfg); err != nil {
		t.Fatalf("Open rejected a valid manifest: %v", err)
	}
}

// A shard file that is missing entirely (manifest references it) is corrupt.
func TestMissingShardCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(shardPath(dir, 0)); err != nil {
		t.Fatalf("remove shard: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a missing shard")
	}
}

var errInjected = errors.New("injected persistence failure")

// TestValidateManifestShardCountBoundaries checks the required shard count at
// exact capacity boundaries for one- and two-page shards.
func TestValidateManifestShardCountBoundaries(t *testing.T) {
	for _, pages := range []uint64{1, 2} {
		C := pages * slotsPerHashPage
		cfg := Config{
			MaxShardBytes:   shardBytes(int(pages), defaultSeqBlockRecords),
			SeqBlockRecords: defaultSeqBlockRecords,
		}
		for _, committed := range []uint64{0, 1, C - 1, C, C + 1, 2*C - 1, 2 * C, 2*C + 1} {
			required := uint64(0)
			if committed != 0 {
				required = 1 + (committed-1)/C
			}
			good := manifest{
				pagesPerShard:   pages,
				seqBlockRecords: uint64(defaultSeqBlockRecords),
				committed:       committed,
				shardCount:      required,
				generation:      1,
			}
			if err := validateManifest(good, cfg); err != nil {
				t.Fatalf("pages=%d committed=%d: valid manifest rejected: %v", pages, committed, err)
			}
			if required > 0 {
				under := good
				under.shardCount = required - 1
				if err := validateManifest(under, cfg); err == nil {
					t.Fatalf("pages=%d committed=%d: accepted under count %d", pages, committed, under.shardCount)
				}
			}
			over := good
			over.shardCount = required + 1
			if err := validateManifest(over, cfg); err == nil {
				t.Fatalf("pages=%d committed=%d: accepted over count %d", pages, committed, over.shardCount)
			}
		}
	}
}

// TestCommittedInShardBoundaries checks the subtraction-based per-shard count,
// including reserved ImageID 0 and MaxUint64 lastID.
func TestCommittedInShardBoundaries(t *testing.T) {
	C := uint64(4) * slotsPerHashPage // one shard, includes reserved ID 0
	max := ^uint64(0)
	cases := []struct {
		firstID, capacity, lastID, want uint64
	}{
		{0, C, 0, 0},
		{0, C, 1, 1},
		{0, C, C - 1, C - 1},
		{0, C, C, C - 1}, // ID C lives in the next shard
		{0, C, C + 1, C - 1},
		{0, C, max - 1, C - 1},
		{0, C, max, C - 1},
		{C, C, C - 1, 0},
		{C, C, C, 1},
		{C, C, 2*C - 1, C},
		{C, C, 2 * C, C},
		{C, C, max, C},
		{0, 0, 5, 0},
		{0, 1, 5, 0}, // capacity 1 is entirely the reserved slot
	}
	for _, c := range cases {
		if got := committedInShard(c.firstID, c.capacity, c.lastID); got != c.want {
			t.Fatalf("committedInShard(%d,%d,%d)=%d want %d", c.firstID, c.capacity, c.lastID, got, c.want)
		}
	}
}

// TestPersistenceFailureInjection fails AppendBatch at each durability stage and
// verifies pre-commit failures roll back while post-commit failures keep the new
// committed state.
func TestPersistenceFailureInjection(t *testing.T) {
	stages := []struct {
		name      string
		stage     persistStage
		committed bool
	}{
		{"canonical-sync", stageCanonicalSync, false},
		{"shard-meta-write", stageShardMetaWrite, false},
		{"shard-meta-sync", stageShardMetaSync, false},
		{"manifest", stageManifest, false},
		{"wal-reset", stageWALReset, true},
	}
	for _, tc := range stages {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			cfg.Durability = common.DurabilitySync
			st, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := st.AppendBatch(randRecords(1, 8)); err != nil {
				t.Fatalf("AppendBatch: %v", err)
			}
			if err := st.Checkpoint(); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			beforeLen := st.Len()
			beforeGen := st.Generation()

			persistentFail = func(s persistStage) error {
				if s == tc.stage {
					return errInjected
				}
				return nil
			}
			_, err = st.AppendBatch(randRecords(2, 8))
			persistentFail = nil
			if !errors.Is(err, errInjected) {
				t.Fatalf("AppendBatch error = %v, want injected", err)
			}
			if tc.committed {
				if st.Len() != beforeLen+8 || st.Generation() != beforeGen+1 {
					t.Fatalf("post-commit failure changed state: len=%d gen=%d", st.Len(), st.Generation())
				}
			} else if st.Len() != beforeLen || st.Generation() != beforeGen {
				t.Fatalf("pre-commit failure changed state: len=%d gen=%d", st.Len(), st.Generation())
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reopened, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer reopened.Close()
			want := beforeLen
			if tc.committed {
				want = beforeLen + 8
			}
			if reopened.Len() != want {
				t.Fatalf("reopened len=%d, want %d", reopened.Len(), want)
			}
		})
	}
}

// TestStaleWALAfterManifestCommit commits the manifest but fails the WAL reset,
// then verifies the already-committed WAL batch is skipped on reopen and the WAL
// can be cleaned afterwards.
func TestStaleWALAfterManifestCommit(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Durability = common.DurabilitySync
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(1, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	base := st.Generation()
	target := randRecords(2, 1)[0]
	if err := st.Upsert(2, target); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	persistentFail = func(s persistStage) error {
		if s == stageWALReset {
			return errInjected
		}
		return nil
	}
	err = st.Checkpoint()
	persistentFail = nil
	if !errors.Is(err, errInjected) {
		t.Fatalf("Checkpoint error = %v, want injected", err)
	}
	if st.Generation() != base+1 {
		t.Fatalf("generation = %d, want %d", st.Generation(), base+1)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Generation() != base+1 {
		t.Fatalf("reopened generation = %d, want %d", reopened.Generation(), base+1)
	}
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("reopened Get(2) = %+v, want %+v", got, target)
	}
	if err := reopened.Checkpoint(); err != nil {
		t.Fatalf("cleanup checkpoint: %v", err)
	}
	if size, _ := journalSize(dir); size != 0 {
		t.Fatalf("WAL not reset: %d bytes", size)
	}
}

// TestAppendUpdateInteraction covers updates to durable and newly appended
// (uncheckpointed) records followed by a crash without checkpoint.
func TestAppendUpdateInteraction(t *testing.T) {
	t.Run("update-durable-id", func(t *testing.T) {
		dir := t.TempDir()
		cfg := testConfig()
		st, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.AppendBatch(randRecords(1, 8)); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		if err := st.Checkpoint(); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		base := st.Generation()
		if _, err := st.AppendBatch(randRecords(2, 4)); err != nil { // uncheckpointed
			t.Fatalf("AppendBatch: %v", err)
		}
		target := randRecords(3, 1)[0]
		if err := st.Upsert(2, target); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reopened, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		if reopened.Len() != 8 {
			t.Fatalf("reopened len = %d, want 8 (append lost)", reopened.Len())
		}
		if got, ok := reopened.Get(2); !ok || got != target {
			t.Fatalf("reopened Get(2) = %+v, want %+v", got, target)
		}
		if base >= reopened.Generation() {
			t.Fatalf("generation did not advance: base %d, got %d", base, reopened.Generation())
		}
	})

	t.Run("update-uncheckpointed-appended-id", func(t *testing.T) {
		dir := t.TempDir()
		cfg := testConfig()
		st, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.AppendBatch(randRecords(4, 8)); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		if err := st.Checkpoint(); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		if _, err := st.AppendBatch(randRecords(5, 4)); err != nil { // ids 9..12, uncheckpointed
			t.Fatalf("AppendBatch: %v", err)
		}
		target := randRecords(6, 1)[0]
		if err := st.Upsert(9, target); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reopened, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("reopen after updating an uncheckpointed id: %v", err)
		}
		defer reopened.Close()
		if _, ok := reopened.Get(9); ok {
			t.Fatal("reopened Get(9) returned a record whose append was not committed")
		}
		if reopened.Len() != 8 {
			t.Fatalf("reopened len = %d, want 8", reopened.Len())
		}
	})
}

// TestBatchAtomicRecovery interrupts an UpsertBatch at each apply position and
// verifies recovery produces the complete batch.
func TestBatchAtomicRecovery(t *testing.T) {
	for applied := 0; applied <= 3; applied++ {
		t.Run(map[int]string{0: "before-apply", 1: "after-first", 2: "after-middle", 3: "after-last"}[applied], func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			st, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := st.AppendBatch(randRecords(7, 8)); err != nil {
				t.Fatalf("AppendBatch: %v", err)
			}
			if err := st.Checkpoint(); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			targets := []Record{randRecords(8, 1)[0], randRecords(9, 1)[0], randRecords(10, 1)[0]}
			updates := []Update{{ImageID: 2, Record: targets[0]}, {ImageID: 3, Record: targets[1]}, {ImageID: 4, Record: targets[2]}}
			appendTestBatch(t, st, updates...)
			sh := st.shards[0]
			pi := uint64(0)
			for i := 0; i < applied; i++ {
				st.applyPointNoSeq(sh, pi, slotOf(uint64(updates[i].ImageID)), updates[i].Record)
			}
			// A partially written tuple for the next position: pHash and the
			// first PDQ word only, simulating a crash mid-tuple.
			if applied < 3 {
				next := updates[applied]
				s := slotOf(uint64(next.ImageID))
				atomic.StoreUint64(sh.phash(pi, s), next.Record.PHash)
				w := hashWords(&next.Record.PDQ)
				atomic.StoreUint64(sh.pdqWord(pi, s, 0), w[0])
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reopened, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer reopened.Close()
			for _, u := range updates {
				got, ok := reopened.Get(u.ImageID)
				if !ok || got != u.Record {
					t.Fatalf("recovered Get(%d) = %+v, want %+v (applied=%d)", u.ImageID, got, u.Record, applied)
				}
			}
		})
	}
}

// TestShardBoundaryConcurrentReaders appends across a shard boundary while
// readers search and Get, verifying no missing-shard panic or invalid access.
func TestShardBoundaryConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	records := randRecords(11, slotsPerHashPage-32)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = st.SearchPHash(nil, records[0].PHash, 0)
				_ = st.SearchPDQ(nil, &records[0].PDQ, 0)
				_, _ = st.Get(1)
			}
		}()
	}
	runtime.Gosched()
	// Each append either grows within shard 0 or crosses into a new shard.
	cross := randRecords(12, 64)
	for i := 0; i < 200; i++ {
		if _, err := st.AppendBatch(cross); err != nil {
			t.Fatalf("AppendBatch crossing boundary: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if _, ok := st.Get(st.Len()); !ok {
		t.Fatal("last committed record not readable after shard growth")
	}
}

// TestAppendPreflightFailureLeavesStateUnchanged induces a shard-creation
// failure by making the directory read-only after one shard exists, then
// appending across a shard boundary. Logical state must be unchanged; any
// already-created shard is physical capacity only.
func TestAppendPreflightFailureLeavesStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.Append(randRecords(1, 1)[0]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	beforeLen := st.Len()
	beforeGen := st.Generation()

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o755)
	// IDs 2..slotsPerHashPage+1 span past shard 0 (one page), so shard 1 must be
	// created; creation fails because the directory is read-only.
	recs := randRecords(2, slotsPerHashPage)
	if _, err := st.AppendBatch(recs); err == nil {
		t.Skip("could not induce a shard-creation failure (running privileged?)")
	}
	if st.Len() != beforeLen {
		t.Fatalf("Len changed after failed preflight: %d -> %d", beforeLen, st.Len())
	}
	if st.Generation() != beforeGen {
		t.Fatalf("generation changed after failed preflight: %d -> %d", beforeGen, st.Generation())
	}
}

// TestZeroAllocationOperations protects the allocation behavior of every hot
// operation with testing.AllocsPerRun.
func TestZeroAllocationOperations(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	records := randRecords(300, 20_000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	phDst := make([]Result, 0, 8192)
	pdDst := make([]Result, 0, 8192)
	batch := records[:64]
	single := records[0]
	updates := make([]Update, 64)
	for i := range updates {
		updates[i] = Update{ImageID: common.ImageID(1 + i), Record: records[i]}
	}

	check := func(name string, f func()) {
		t.Helper()
		if n := testing.AllocsPerRun(300, f); n != 0 {
			t.Fatalf("%s: %.2f allocs/op, want 0", name, n)
		}
	}
	check("Append", func() { _, _ = st.Append(single) })
	check("AppendBatch64", func() { _, _ = st.AppendBatch(batch) })
	check("Upsert", func() { _ = st.Upsert(1, single) })
	check("UpsertBatch64", func() { _ = st.UpsertBatch(updates) })
	check("Get", func() { _, _ = st.Get(1) })
	check("Quality", func() { _, _ = st.Quality(1) })
	check("SearchPHash", func() { phDst = st.SearchPHash(phDst[:0], single.PHash, 0) })
	check("SearchPDQ", func() { pdDst = st.SearchPDQ(pdDst[:0], &single.PDQ, 0) })
	check("SearchPDQBulk", func() { pdDst = st.SearchPDQBulk(pdDst[:0], &single.PDQ, 0) })
}

// TestUpsertBatchDuplicateThreeLastWins checks the documented last-wins rule.
func TestUpsertBatchDuplicateThreeLastWins(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(301, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	a, b, c := randRecords(302, 1)[0], randRecords(303, 1)[0], randRecords(304, 1)[0]
	if err := st.UpsertBatch([]Update{
		{ImageID: 2, Record: a},
		{ImageID: 2, Record: b},
		{ImageID: 2, Record: c},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if got, _ := st.Get(2); got != c {
		t.Fatalf("duplicate last-wins: got %+v, want %+v", got, c)
	}
}

// TestCloseRaceStress repeatedly races all read APIs with Close under -race.
func TestCloseRaceStress(t *testing.T) {
	for round := 0; round < 20; round++ {
		st, err := Open(t.TempDir(), testConfig())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.AppendBatch(randRecords(int64(round+1), 1000)); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var zeroPDQ PDQHash
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = st.Get(1)
					_, _ = st.Quality(1)
					_ = st.SearchPHash(nil, 0x1234, 8)
					_ = st.SearchPDQ(nil, &zeroPDQ, 8)
				}
			}()
		}
		runtime.Gosched()
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		close(stop)
		wg.Wait()
	}
}
