package simhash

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

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

// TestJournalCompletesInterruptedUpsert simulates a crash after the journal
// record is durable but before the canonical columns are updated.
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
	jr := journalRecord{imageID: 2, phash: target.PHash, pdq: target.PDQ, quality: target.Quality}
	if err := st.journal.appendRecord(jr, true); err != nil {
		t.Fatalf("journal: %v", err)
	}
	// No apply: simulate crash.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, ok := reopened.Get(2)
	if !ok || got != target {
		t.Fatalf("recovered Get(2) = %+v, want %+v", got, target)
	}
}

// TestJournalRepairsTornApply simulates a crash mid-apply (pHash and one PDQ
// word written, sequence left odd) and asserts recovery produces a complete,
// consistent tuple and resets the sequence.
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
	jr := journalRecord{imageID: 3, phash: target.PHash, pdq: target.PDQ, quality: target.Quality}
	if err := st.journal.appendRecord(jr, true); err != nil {
		t.Fatalf("journal: %v", err)
	}
	// Torn apply: pHash and one PDQ word only, and leave the sequence odd.
	sh := st.shards[0]
	pi := uint64(0)
	atomic.StoreUint64(sh.phash(pi, 3), jr.phash)
	words := hashWords(&jr.pdq)
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

// TestJournalTornTailRepaired appends a partial frame after a valid record and
// asserts recovery keeps the valid record and truncates the tail.
// TestJournalRecoveryStages crashes at every journal/update stage and asserts
// recovery yields either the old record (journal absent) or a complete new
// tuple, with the sequence reset even.
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
			jr := journalRecord{imageID: 2, phash: target.PHash, pdq: target.PDQ, quality: target.Quality}
			if stage.journal {
				if err := st.journal.appendRecord(jr, true); err != nil {
					t.Fatalf("journal: %v", err)
				}
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
				shift := qualityShift(2)
				qw := sh.qualityWord(pi, 2)
				atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(target.Quality)<<shift))
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

// TestCloseDoesNotPersist documents the radixhash-style contract: Close unmaps
// but does not checkpoint, so uncommitted appends are lost on reopen.
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
	jr := journalRecord{imageID: 1, phash: target.PHash, pdq: target.PDQ, quality: target.Quality}
	if err := st.journal.appendRecord(jr, true); err != nil {
		t.Fatalf("journal: %v", err)
	}
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
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Fabricate a full, framed record with a bad payload CRC.
	rec := make([]byte, journalHeaderBytes+journalPayloadBytes)
	put := func(off int, v uint32) {
		rec[off] = byte(v)
		rec[off+1] = byte(v >> 8)
		rec[off+2] = byte(v >> 16)
		rec[off+3] = byte(v >> 24)
	}
	put(0, journalMagic)
	rec[4] = byte(journalVersion)
	put(8, journalPayloadBytes)
	// payload left zero, CRC will not match the zeroed contents' checksum only
	// if we set a wrong CRC explicitly:
	put(journalHeaderBytes+journalCRCOffset, 0xDEADBEEF)
	path := filepath.Join(dir, journalFilename)
	if err := os.WriteFile(path, rec, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a corrupt journal payload")
	}
}

func TestSequenceResetOnOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(8, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	v := st.view.Load()
	sh, pi := v.shardForHashPage(0)
	atomic.StoreUint64(sh.seqPtr(pi, 0), 7) // odd, as after a crash
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
