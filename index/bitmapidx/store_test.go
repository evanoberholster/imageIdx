package bitmapidx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
)

func TestNamespaceCrashWriter(t *testing.T) {
	dir := os.Getenv("BITMAPIDX_CRASH_TEST_DIR")
	if dir == "" {
		return
	}
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 42, Value: EnumStringValue("durable")}) })
	os.Exit(0) // crash boundary: no checkpoint, Close, or deferred cleanup
}

func TestNamespaceRecoveryAfterProcessExit(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNamespaceCrashWriter$")
	cmd.Env = append(os.Environ(), "BITMAPIDX_CRASH_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s: %v", out, err)
	}
	s, err := OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ := s.Lookup(100)
	if ok, err := index.Contains(42, EnumStringValue("durable")); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestNamespaceManifestFailureRequiresRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 1, Value: EnumStringValue("a")}) })
	// Prevent writing the manifest temporary after successor files are ready.
	temporary := filepath.Join(dir, "manifest.tmp")
	if err := os.Mkdir(temporary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	recovery := index.NewBatch()
	defer recovery.Close()
	if err := recovery.Add(Entry{ImageID: 2, Value: EnumStringValue("b")}); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, EnumStringValue("a")); !slices.Equal(got, []common.ImageID{1}) {
		t.Fatal(got)
	}
	// An unpublished successor exists; the next checkpoint safely replaces it.
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func TestNamespacePersistentCollisions(t *testing.T) {
	previous := hashCanonical
	hashCanonical = func(t common.Namespace, _ common.NamespaceKind, _ []byte) NamespaceKey {
		return NamespaceKey{Namespace: t, Hash: 99}
	}
	defer func() { hashCanonical = previous }()
	dir := t.TempDir()
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	commit(t, index, func(bt *Batch) error {
		return bt.Add(Entry{ImageID: 1, Value: EnumStringValue("a")}, Entry{ImageID: 2, Value: EnumStringValue("b")})
	})
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	commit(t, index, func(bt *Batch) error { return bt.Remove(Entry{ImageID: 1, Value: EnumStringValue("a")}) })
	if err := index.Vacuum(); err != nil {
		t.Fatal(err)
	}
	commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 3, Value: EnumStringValue("c")}) })
	s.Close()
	s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, EnumStringValue("b")); !slices.Equal(got, []common.ImageID{2}) {
		t.Fatal(got)
	}
	if got := appendIDs(t, index, EnumStringValue("c")); !slices.Equal(got, []common.ImageID{3}) {
		t.Fatal(got)
	}
}

func TestPersistentNamespacesAllKinds(t *testing.T) {
	namespaces := []NamespaceSpec{enumNamespace(100), boolNamespace(101), textNamespace(102), continuousNamespace(103)}
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewStore(dir, namespaces, NamespaceConfig{}, common.DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			tags, _ := s.Lookup(100)
			flags, _ := s.Lookup(101)
			text, _ := s.Lookup(102)
			numbers, _ := s.Lookup(103)
			for id := common.ImageID(1); id <= 6; id++ {
				commit(t, tags, func(bt *Batch) error {
					return bt.Add(Entry{ImageID: id, Value: EnumStringValue("tag")})
				})
			}
			for _, entry := range []Entry{{ImageID: 10, Value: EnumBytesValue(nil)}, {ImageID: 11, Value: EnumStringValue("dead")}} {
				commit(t, tags, func(bt *Batch) error { return bt.Add(entry) })
			}
			commit(t, tags, func(bt *Batch) error { return bt.Remove(Entry{ImageID: 11, Value: EnumStringValue("dead")}) })
			commit(t, flags, func(bt *Batch) error { return bt.Add(Entry{ImageID: 1, Value: BoolValue(false)}) })
			commit(t, text, func(bt *Batch) error {
				return bt.ReplaceText(TextReplacement{ImageID: 1, After: []string{"golden bridge"}})
			})
			commit(t, text, func(bt *Batch) error {
				return bt.ReplaceText(TextReplacement{ImageID: 1, Before: []string{"golden bridge"}, After: []string{"golden park"}})
			})
			commit(t, numbers, func(bt *Batch) error {
				return bt.Add(Entry{ImageID: 1, Value: IntValue(math.MinInt64)}, Entry{ImageID: 2, Value: IntValue(math.MaxInt64)})
			})
			commit(t, numbers, func(bt *Batch) error {
				if err := bt.Remove(Entry{ImageID: 1, Value: IntValue(math.MinInt64)}); err != nil {
					return err
				}
				return bt.Add(Entry{ImageID: 1, Value: IntValue(0)})
			})
			if checkpoint {
				if err := s.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tags, _ = s.Lookup(100)
			flags, _ = s.Lookup(101)
			text, _ = s.Lookup(102)
			numbers, _ = s.Lookup(103)
			if got := appendIDs(t, tags, EnumStringValue("tag")); len(got) != 6 {
				t.Fatal(got)
			}
			if got := appendIDs(t, tags, EnumBytesValue(nil)); !slices.Equal(got, []common.ImageID{10}) {
				t.Fatal(got)
			}
			if present, err := flags.Contains(1, BoolValue(false)); err != nil || !present {
				t.Fatal(present, err)
			}
			if got, err := text.MatchText("golden park"); err != nil || !got.Contains(1) {
				t.Fatal(got, err)
			}
			if got, err := text.MatchText("bridge"); err != nil || !got.IsEmpty() {
				t.Fatal(got, err)
			}
			if got, err := numbers.Range(AtMost(0)); err != nil || !slices.Equal(got.AppendTo(nil), []common.ImageID{1}) {
				t.Fatal(got, err)
			}
			query, err := tags.Compile(EnumStringValue("tag"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Vacuum(); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := tags.KeyForValue(EnumStringValue("dead")); err != nil || ok {
				t.Fatal("dead dictionary survived", err)
			}
			result, err := query.Match()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if !result.Contains(6) {
				t.Fatal("result did not survive unmap")
			}
		})
	}
}

func TestNamespaceWALTailAndValidation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			index, _ := s.Lookup(100)
			commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 1, Value: EnumStringValue("a")}) })
			path := s.walPath(s.generation)
			s.Close()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				data[len(data)-1] ^= 1
			} else {
				tail := append([]byte(nil), data[:namespaceWALHeaderSize+1]...)
				binary.LittleEndian.PutUint64(tail[32:40], uint64(len(data)))
				binary.LittleEndian.PutUint32(tail[60:64], crc32.ChecksumIEEE(tail[:60]))
				data = append(data, tail...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilitySync)
			if corrupt {
				if !errors.Is(err, ErrCorruptWAL) {
					if s != nil {
						s.Close()
					}
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			index, _ = s.Lookup(100)
			commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 2, Value: EnumStringValue("b")}) })
			s.Close()
			s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			index, _ = s.Lookup(100)
			if got := appendIDs(t, index, EnumStringValue("b")); !slices.Equal(got, []common.ImageID{2}) {
				t.Fatal(got)
			}
		})
	}
}

func TestIndexBoundsAndFailedWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ := s.Lookup(100)
	entries := make([]Entry, MaxNamespaceBatchUpdates+1)
	for n := range entries {
		entries[n] = Entry{ImageID: uint64(n + 1), Value: EnumStringValue("oversized")}
	}
	oversized := index.NewBatch()
	defer oversized.Close()
	if err := oversized.Add(entries...); err != nil {
		t.Fatal(err)
	}
	if err := oversized.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatal(err)
	}
	if _, ok, _ := index.KeyForValue(EnumStringValue("oversized")); ok {
		t.Fatal("rejected batch dictionary published")
	}
	commit(t, index, func(bt *Batch) error { return bt.Add(entries[:MaxNamespaceBatchUpdates]...) })
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	s.wal.Close()
	failed := index.NewBatch()
	defer failed.Close()
	if err := failed.Add(Entry{ImageID: 99, Value: EnumStringValue("failed")}); err != nil {
		t.Fatal(err)
	}
	if err := failed.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if _, ok, _ := index.KeyForValue(EnumStringValue("failed")); ok {
		t.Fatal("failed WAL published dictionary")
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	later := index.NewBatch()
	defer later.Close()
	if err := later.Add(Entry{ImageID: 98, Value: EnumStringValue("later")}); err != nil {
		t.Fatal(err)
	}
	if err := later.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := index.Barrier(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
}

func TestNamespaceConcurrentCheckpointAndReaders(t *testing.T) {
	dir := t.TempDir()
	config := NamespaceConfig{ActiveTarget: 2, HardDeltaLimit: 8, CompactDelay: time.Millisecond}
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, config, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	var seed []Entry
	for id := common.ImageID(1001); id <= 1040; id++ {
		seed = append(seed, Entry{ImageID: id, Value: EnumStringValue("shared")})
	}
	commit(t, index, func(bt *Batch) error { return bt.Add(seed...) })
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					index.Contains(1, EnumStringValue("v-1"))
					q, _ := index.MatchValues(EnumStringValue("shared"))
					q.Contains(1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 12; n++ {
			if err := s.Vacuum(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for n := 1; n <= 100; n++ {
		commit(t, index, func(bt *Batch) error {
			return bt.Add(Entry{ImageID: uint64(n), Value: EnumStringValue(fmt.Sprintf("v-%d", n))},
				Entry{ImageID: uint64(n), Value: EnumStringValue("shared")})
		})
	}
	close(done)
	wg.Wait()
	s.Close()
	s, err = OpenStore(dir, config, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, EnumStringValue("shared")); len(got) != 140 {
		t.Fatalf("shared posting lost members: %d", len(got))
	}
	for n := 1; n <= 100; n++ {
		if ok, err := index.Contains(uint64(n), EnumStringValue(fmt.Sprintf("v-%d", n))); err != nil || !ok {
			t.Fatalf("missing %d: %v", n, err)
		}
	}
}

func TestIndexOwnershipAndSnapshotCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenStore(dir, NamespaceConfig{}, common.DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	if err := index.Close(); !errors.Is(err, ErrOwnedIndex) {
		t.Fatal(err)
	}
	path := s.snapshotPath(s.generation)
	s.Close()
	if other, err := NewStore(dir, nil, NamespaceConfig{}, common.DurabilityOS); !errors.Is(err, ErrStoreExists) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenStore(dir, NamespaceConfig{}, common.DurabilityOS); !errors.Is(err, ErrCorruptSegment) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
}

func TestIndexRejectsForeignFiles(t *testing.T) {
	build := func(t *testing.T, dir string, id common.ImageID) (snapshot, wal string) {
		t.Helper()
		s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		index, _ := s.Lookup(100)
		commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: id, Value: EnumStringValue("v")}) })
		snapshot, wal = s.snapshotPath(s.generation), s.walPath(s.generation)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return snapshot, wal
	}
	copyFile := func(t *testing.T, src, dst string) {
		t.Helper()
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("wal", func(t *testing.T) {
		dirA, dirB := t.TempDir(), t.TempDir()
		_, walA := build(t, dirA, 1)
		_, walB := build(t, dirB, 2)
		copyFile(t, walB, walA)
		s, err := OpenStore(dirA, NamespaceConfig{}, common.DurabilityOS)
		if s != nil {
			s.Close()
		}
		if !errors.Is(err, ErrCorruptWAL) {
			t.Fatalf("foreign WAL error = %v, want ErrCorruptWAL", err)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		dirC, dirD := t.TempDir(), t.TempDir()
		snapC, _ := build(t, dirC, 1)
		snapD, _ := build(t, dirD, 2)
		copyFile(t, snapD, snapC)
		s, err := OpenStore(dirC, NamespaceConfig{}, common.DurabilityOS)
		if s != nil {
			s.Close()
		}
		if !errors.Is(err, ErrCorruptSegment) {
			t.Fatalf("foreign snapshot error = %v, want ErrCorruptSegment", err)
		}
	})
}

// TestNamespaceCheckpointCrashBoundaries interrupts a checkpoint at each stage
// that can leave partial successor state, verifying open ignores unpublished
// generations and that a failed segment write stays recoverable.
func TestNamespaceCheckpointCrashBoundaries(t *testing.T) {
	t.Run("unpublished successor files ignored", func(t *testing.T) {
		dir := t.TempDir()
		s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		index, _ := s.Lookup(100)
		commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 1, Value: EnumStringValue("a")}) })
		// Crash after successor files exist but before the manifest publishes
		// them: a half-written successor segment, WAL, and temporary file.
		successor := s.generation + 1
		for _, path := range []string{
			s.snapshotPath(successor),
			s.walPath(successor),
			s.snapshotPath(successor) + ".tmp",
		} {
			if err := os.WriteFile(path, []byte("garbage"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
		if err != nil {
			t.Fatalf("open with unpublished successor: %v", err)
		}
		index, _ = s.Lookup(100)
		if got := appendIDs(t, index, EnumStringValue("a")); !slices.Equal(got, []common.ImageID{1}) {
			t.Fatalf("recovered membership = %v, want [1]", got)
		}
		// The next checkpoint must replace the orphan generation.
		if err := s.Checkpoint(); err != nil {
			t.Fatalf("checkpoint after orphan: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("segment write failure recoverable", func(t *testing.T) {
		dir := t.TempDir()
		s, err := NewStore(dir, []NamespaceSpec{enumNamespace(100)}, NamespaceConfig{}, common.DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		index, _ := s.Lookup(100)
		commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 1, Value: EnumStringValue("a")}) })
		// Block the temporary segment path so the checkpoint fails before any
		// successor is published or the manifest is replaced.
		blocked := s.snapshotPath(s.generation+1) + ".tmp"
		if err := os.Mkdir(blocked, 0700); err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(); err == nil {
			t.Fatal("checkpoint with blocked temporary path succeeded")
		}
		// The live store stays usable and later writes replay.
		commit(t, index, func(bt *Batch) error { return bt.Add(Entry{ImageID: 2, Value: EnumStringValue("b")}) })
		if err := os.Remove(blocked); err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(); err != nil {
			t.Fatalf("checkpoint after unblocking: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		index, _ = s.Lookup(100)
		if got := appendIDs(t, index, EnumStringValue("b")); !slices.Equal(got, []common.ImageID{2}) {
			t.Fatalf("recovered membership = %v, want [2]", got)
		}
	})
}
