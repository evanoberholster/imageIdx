package simhash

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

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
				_ = st.SearchPDQ(nil, records[0].PDQ, 0)
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
