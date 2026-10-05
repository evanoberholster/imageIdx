package simhash

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// appendTestBatch writes a redo batch frame without applying it, simulating a
// crash point at or after WAL publication.
func appendTestBatch(t *testing.T, st *Store, updates ...Update) {
	t.Helper()
	if err := st.journal.appendBatch(st.generation, st.generation+1, updates, true); err != nil {
		t.Fatalf("appendBatch: %v", err)
	}
}

func TestUpsertAdvancesGeneration(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(1, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	afterAppend := st.Generation()
	if err := st.Upsert(1, randRecords(2, 1)[0]); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.Upsert(2, randRecords(9, 1)[0]); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if st.Generation() != afterAppend+2 {
		t.Fatalf("Upsert generation = %d, want %d", st.Generation(), afterAppend+2)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	committedGen := st.Generation()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Generation() != committedGen {
		t.Fatalf("reopened generation = %d, want %d", reopened.Generation(), committedGen)
	}
}

// TestUpsertBatchOneGeneration asserts one batch is one logical generation.
func TestUpsertBatchOneGeneration(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(50, 200)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	before := st.Generation()
	updates := make([]Update, 64)
	for i := range updates {
		updates[i] = Update{ImageID: common.ImageID(i + 1), Record: randRecords(int64(100+i), 1)[0]}
	}
	if err := st.UpsertBatch(updates); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if st.Generation() != before+1 {
		t.Fatalf("generation = %d, want %d", st.Generation(), before+1)
	}
}

// TestUpsertBatchDuplicateLastWins checks duplicate ImageIDs resolve to the last
// update in the batch.
func TestUpsertBatchDuplicateLastWins(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(60, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	first := randRecords(61, 1)[0]
	last := randRecords(62, 1)[0]
	if err := st.UpsertBatch([]Update{
		{ImageID: 2, Record: first},
		{ImageID: 3, Record: randRecords(63, 1)[0]},
		{ImageID: 2, Record: last},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if got, _ := st.Get(2); got != last {
		t.Fatalf("Get(2) = %+v, want last %+v", got, last)
	}
}

// TestUpsertBatchMatchesIndividual asserts a batch and the equivalent sequence
// of single Upserts produce identical state.
func TestUpsertBatchMatchesIndividual(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(70, 512)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	updates := make([]Update, 64)
	for i := range updates {
		updates[i] = Update{ImageID: common.ImageID(1 + (i*7)%512), Record: randRecords(int64(200+i), 1)[0]}
	}
	if err := st.UpsertBatch(updates); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	// Rebuild with single Upserts in a fresh store and compare every id.
	dir2 := t.TempDir()
	st2, err := Open(dir2, cfg)
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	defer st2.Close()
	if _, err := st2.AppendBatch(randRecords(70, 512)); err != nil {
		t.Fatalf("AppendBatch2: %v", err)
	}
	for _, u := range updates {
		if err := st2.Upsert(u.ImageID, u.Record); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	for id := common.ImageID(1); id <= 512; id++ {
		a, _ := st.Get(id)
		b, _ := st2.Get(id)
		if a != b {
			t.Fatalf("id %d: batch %+v != individual %+v", id, a, b)
		}
	}
	st.Close()
}

func TestUpsertBatchTooLarge(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(80, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	updates := make([]Update, maxBatchRecords+1)
	for i := range updates {
		updates[i] = Update{ImageID: 1, Record: Record{}}
	}
	if err := st.UpsertBatch(updates); err != ErrBatchTooLarge {
		t.Fatalf("UpsertBatch = %v, want ErrBatchTooLarge", err)
	}
}

// TestJournalCompletesInterruptedUpsert simulates a crash after the batch frame
// is durable but before the canonical columns are updated.
func TestJournalCompletesInterruptedUpsert(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(1, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	target := randRecords(2, 1)[0]
	appendTestBatch(t, st, Update{ImageID: 2, Record: target})
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("recovered Get(2) = %+v, want %+v", got, target)
	}
}

// TestJournalRepairsTornApply simulates a crash mid-apply with an odd sequence.
func TestJournalRepairsTornApply(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(3, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	target := randRecords(4, 1)[0]
	appendTestBatch(t, st, Update{ImageID: 3, Record: target})
	sh := st.shards[0]
	pi := uint64(0)
	atomic.StoreUint64(sh.phash(pi, 3), target.PHash)
	words := hashWords(&target.PDQ)
	atomic.StoreUint64(sh.pdqWord(pi, 3, 0), words[0])
	atomic.StoreUint64(sh.seqPtr(pi, 0), 1)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, _ := reopened.Get(3); got != target {
		t.Fatalf("recovered Get(3) = %+v, want %+v", got, target)
	}
	v := reopened.view.Load()
	sh2, pi2 := v.shardForHashPage(0)
	if seq := atomic.LoadUint64(sh2.seqPtr(pi2, 0)); seq != 0 {
		t.Fatalf("sequence after recovery = %d, want 0", seq)
	}
}

// TestJournalRecoveryStages crashes at every journal/update stage and asserts
// recovery yields the old record (no journal) or a complete new tuple.
func TestJournalRecoveryStages(t *testing.T) {
	stages := []struct {
		name     string
		journal  bool
		phash    bool
		pdqWords int
		quality  bool
		seqOdd   bool
	}{
		{"beforeJournal", false, false, 0, false, false},
		{"journalOnly", true, false, 0, false, true},
		{"afterPHash", true, true, 0, false, true},
		{"afterPDQWord0", true, true, 1, false, true},
		{"afterPDQWord1", true, true, 2, false, true},
		{"afterPDQWord2", true, true, 3, false, true},
		{"afterPDQWord3", true, true, 4, false, true},
		{"afterQuality", true, true, 4, true, false},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			st, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			original := randRecords(11, 4)
			if _, err := st.AppendBatch(original); err != nil {
				t.Fatalf("AppendBatch: %v", err)
			}
			if err := st.Checkpoint(); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			target := randRecords(12, 1)[0]
			if stage.journal {
				appendTestBatch(t, st, Update{ImageID: 2, Record: target})
			}
			sh := st.shards[0]
			pi := uint64(0)
			if stage.phash {
				atomic.StoreUint64(sh.phash(pi, 2), target.PHash)
			}
			words := hashWords(&target.PDQ)
			for k := 0; k < stage.pdqWords; k++ {
				atomic.StoreUint64(sh.pdqWord(pi, 2, k), words[k])
			}
			if stage.quality {
				st.storeQuality(sh, pi, 2, target.Quality)
			}
			if stage.seqOdd {
				atomic.StoreUint64(sh.seqPtr(pi, 0), 1)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reopened, err := Open(dir, cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer reopened.Close()
			want := target
			if !stage.journal {
				want = original[1]
			}
			if got, ok := reopened.Get(2); !ok || got != want {
				t.Fatalf("Get(2) = %+v, want %+v", got, want)
			}
			v := reopened.view.Load()
			sh2, pi2 := v.shardForHashPage(0)
			if seq := atomic.LoadUint64(sh2.seqPtr(pi2, 0)); seq != 0 {
				t.Fatalf("sequence = %d, want 0", seq)
			}
		})
	}
}

// TestCloseDoesNotPersist documents the radixhash-style contract.
func TestCloseDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(13, 10)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Close(); err != nil { // deliberately no Checkpoint
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 0 {
		t.Fatalf("Close persisted %d records; want 0 without Checkpoint", reopened.Len())
	}
}

func TestJournalTornTailRepaired(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(5, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	target := randRecords(6, 1)[0]
	appendTestBatch(t, st, Update{ImageID: 1, Record: target})
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(dir, journalFilename)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := file.Write([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	_ = file.Close()
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, _ := reopened.Get(1); got != target {
		t.Fatalf("recovered Get(1) = %+v, want %+v", got, target)
	}
}

func TestJournalCorruptPayloadRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(7, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	appendTestBatch(t, st, Update{ImageID: 1, Record: randRecords(8, 1)[0]})
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Flip the last byte (part of the batch CRC) to corrupt the frame.
	path := filepath.Join(dir, journalFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a corrupt journal payload")
	}
}

// TestJournalRejectsGenerationGap checks the generation-chain validation.
func TestJournalRejectsGenerationGap(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(9, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// A batch whose base does not chain from the manifest generation.
	if err := st.journal.appendBatch(st.generation+5, st.generation+6, []Update{
		{ImageID: 1, Record: randRecords(10, 1)[0]},
	}, true); err != nil {
		t.Fatalf("appendBatch: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a broken generation chain")
	}
}

func TestSequenceResetOnOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(14, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	v := st.view.Load()
	sh, pi := v.shardForHashPage(0)
	atomic.StoreUint64(sh.seqPtr(pi, 0), 7)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2 := reopened.view.Load()
	sh2, pi2 := v2.shardForHashPage(0)
	if got := atomic.LoadUint64(sh2.seqPtr(pi2, 0)); got != 0 {
		t.Fatalf("sequence after open = %d, want 0", got)
	}
}
