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
)

func TestFieldCrashWriter(t *testing.T) {
	dir := os.Getenv("BITMAPIDX_CRASH_TEST_DIR")
	if dir == "" {
		return
	}
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	if err := index.Add(Entry{ImageID: 42, Value: StringValue("durable")}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // crash boundary: no checkpoint, Close, or deferred cleanup
}

func TestFieldRecoveryAfterProcessExit(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFieldCrashWriter$")
	cmd.Env = append(os.Environ(), "BITMAPIDX_CRASH_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s: %v", out, err)
	}
	s, err := OpenPersistentFields(dir, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ := s.Lookup(100)
	if ok, err := index.Contains(42, StringValue("durable")); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestFieldManifestFailureRequiresRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("a")}); err != nil {
		t.Fatal(err)
	}
	// Prevent writing the manifest temporary after successor files are ready.
	temporary := filepath.Join(dir, "manifest.tmp")
	if err := os.Mkdir(temporary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if err := index.Add(Entry{ImageID: 2, Value: StringValue("b")}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	s, err = OpenPersistentFields(dir, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, StringValue("a")); !slices.Equal(got, []ImageID{1}) {
		t.Fatal(got)
	}
	// An unpublished successor exists; the next checkpoint safely replaces it.
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func TestFieldPersistentCollisions(t *testing.T) {
	previous := hashCanonical
	hashCanonical = func(t IndexType, _ Kind, _ []byte) FieldKey { return FieldKey{Type: t, Hash: 99} }
	defer func() { hashCanonical = previous }()
	dir := t.TempDir()
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("a")}, Entry{ImageID: 2, Value: StringValue("b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := index.Remove(Entry{ImageID: 1, Value: StringValue("a")}); err != nil {
		t.Fatal(err)
	}
	if err := index.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if err := index.Add(Entry{ImageID: 3, Value: StringValue("c")}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenPersistentFields(dir, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, StringValue("b")); !slices.Equal(got, []ImageID{2}) {
		t.Fatal(got)
	}
	if got := appendIDs(t, index, StringValue("c")); !slices.Equal(got, []ImageID{3}) {
		t.Fatal(got)
	}
}

func TestPersistentFieldsAllKinds(t *testing.T) {
	fields := []Field{enumField(100), boolField(101), textField(102), continuousField(103), geoFieldFor(104)}
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewPersistentFields(dir, fields, FieldConfig{}, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			tags, _ := s.Lookup(100)
			flags, _ := s.Lookup(101)
			text, _ := s.Lookup(102)
			numbers, _ := s.Lookup(103)
			geo, _ := s.Lookup(104)
			for id := ImageID(1); id <= 6; id++ {
				if err := tags.Add(Entry{ImageID: id, Value: StringValue("tag")}); err != nil {
					t.Fatal(err)
				}
			}
			for _, entry := range []Entry{{ImageID: 10, Value: BytesValue(nil)}, {ImageID: 11, Value: StringValue("dead")}} {
				if err := tags.Add(entry); err != nil {
					t.Fatal(err)
				}
			}
			if err := tags.Remove(Entry{ImageID: 11, Value: StringValue("dead")}); err != nil {
				t.Fatal(err)
			}
			if err := flags.Add(Entry{ImageID: 1, Value: BoolValue(false)}); err != nil {
				t.Fatal(err)
			}
			if err := text.ReplaceText(TextReplacement{ImageID: 1, After: []string{"golden bridge"}}); err != nil {
				t.Fatal(err)
			}
			if err := text.ReplaceText(TextReplacement{ImageID: 1, Before: []string{"golden bridge"}, After: []string{"golden park"}}); err != nil {
				t.Fatal(err)
			}
			if err := numbers.Add(Entry{ImageID: 1, Value: IntValue(math.MinInt64)}, Entry{ImageID: 2, Value: IntValue(math.MaxInt64)}); err != nil {
				t.Fatal(err)
			}
			if err := numbers.Move(Move{ImageID: 1, From: math.MinInt64, To: 0, HasFrom: true, HasTo: true}); err != nil {
				t.Fatal(err)
			}
			cell := sfKey(t)
			if err := geo.Move(Move{ImageID: 1, To: int64(cell)}); err != nil {
				t.Fatal(err)
			}
			if checkpoint {
				if err := s.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenPersistentFields(dir, FieldConfig{}, DurabilityOS)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tags, _ = s.Lookup(100)
			flags, _ = s.Lookup(101)
			text, _ = s.Lookup(102)
			numbers, _ = s.Lookup(103)
			geo, _ = s.Lookup(104)
			if got := appendIDs(t, tags, StringValue("tag")); len(got) != 6 {
				t.Fatal(got)
			}
			if got := appendIDs(t, tags, BytesValue(nil)); !slices.Equal(got, []ImageID{10}) {
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
			if got, err := numbers.Range(AtMost(0)); err != nil || !slices.Equal(got.AppendTo(nil), []ImageID{1}) {
				t.Fatal(got, err)
			}
			if got, err := geo.GeoRanges([]GeoRange{{Start: cell, End: cell}}); err != nil || !got.Contains(1) {
				t.Fatal(got, err)
			}
			query, err := tags.Compile(StringValue("tag"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Vacuum(); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := tags.KeyForValue(StringValue("dead")); err != nil || ok {
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

func TestFieldWALTailAndValidation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			index, _ := s.Lookup(100)
			if err := index.Add(Entry{ImageID: 1, Value: StringValue("a")}); err != nil {
				t.Fatal(err)
			}
			path := s.walPath(s.generation)
			s.Close()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				data[len(data)-1] ^= 1
			} else {
				tail := append([]byte(nil), data[:fieldWALHeaderSize+1]...)
				binary.LittleEndian.PutUint64(tail[32:40], uint64(len(data)))
				binary.LittleEndian.PutUint32(tail[44:48], crc32.ChecksumIEEE(tail[:44]))
				data = append(data, tail...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err = OpenPersistentFields(dir, FieldConfig{}, DurabilitySync)
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
			if err := index.Add(Entry{ImageID: 2, Value: StringValue("b")}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = OpenPersistentFields(dir, FieldConfig{}, DurabilityOS)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			index, _ = s.Lookup(100)
			if got := appendIDs(t, index, StringValue("b")); !slices.Equal(got, []ImageID{2}) {
				t.Fatal(got)
			}
		})
	}
}

func TestFieldStoreBoundsAndFailedWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ := s.Lookup(100)
	entries := make([]Entry, MaxFieldBatchUpdates+1)
	for n := range entries {
		entries[n] = Entry{ImageID: uint64(n + 1), Value: StringValue("oversized")}
	}
	if err := index.Add(entries...); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatal(err)
	}
	if _, ok, _ := index.KeyForValue(StringValue("oversized")); ok {
		t.Fatal("rejected batch dictionary published")
	}
	if err := index.Add(entries[:MaxFieldBatchUpdates]...); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	s.wal.Close()
	if err := index.Add(Entry{ImageID: 99, Value: StringValue("failed")}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if _, ok, _ := index.KeyForValue(StringValue("failed")); ok {
		t.Fatal("failed WAL published dictionary")
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if err := index.Add(Entry{ImageID: 98, Value: StringValue("later")}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
}

func TestFieldConcurrentCheckpointAndReaders(t *testing.T) {
	dir := t.TempDir()
	config := FieldConfig{ActiveTarget: 2, HardDeltaLimit: 8, CompactDelay: time.Millisecond}
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, config, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := s.Lookup(100)
	var seed []Entry
	for id := ImageID(1001); id <= 1040; id++ {
		seed = append(seed, Entry{ImageID: id, Value: StringValue("shared")})
	}
	if err := index.Add(seed...); err != nil {
		t.Fatal(err)
	}
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
					index.Contains(1, StringValue("v-1"))
					q, _ := index.MatchValues(StringValue("shared"))
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
		if err := index.Add(Entry{ImageID: uint64(n), Value: StringValue(fmt.Sprintf("v-%d", n))},
			Entry{ImageID: uint64(n), Value: StringValue("shared")}); err != nil {
			t.Error(err)
		}
	}
	close(done)
	wg.Wait()
	s.Close()
	s, err = OpenPersistentFields(dir, config, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	index, _ = s.Lookup(100)
	if got := appendIDs(t, index, StringValue("shared")); len(got) != 140 {
		t.Fatalf("shared posting lost members: %d", len(got))
	}
	for n := 1; n <= 100; n++ {
		if ok, err := index.Contains(uint64(n), StringValue(fmt.Sprintf("v-%d", n))); err != nil || !ok {
			t.Fatalf("missing %d: %v", n, err)
		}
	}
}

func TestFieldStoreOwnershipAndSnapshotCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentFields(dir, []Field{enumField(100)}, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenPersistentFields(dir, FieldConfig{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
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
	if other, err := NewPersistentFields(dir, nil, FieldConfig{}, DurabilityOS); !errors.Is(err, ErrStoreExists) {
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
	if other, err := OpenPersistentFields(dir, FieldConfig{}, DurabilityOS); !errors.Is(err, ErrCorruptSegment) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
}
