package timeidx

import (
	"errors"
	"testing"
)

// breakWAL closes the index's WAL handle underneath it, so the next WAL
// append fails deterministically. White-box fault injection: it stands in
// for disk errors the test cannot otherwise produce portably.
func breakWAL(t *testing.T, idx *Index) {
	t.Helper()
	idx.persistence.mu.Lock()
	defer idx.persistence.mu.Unlock()
	if err := idx.persistence.wal.Close(); err != nil {
		t.Fatalf("break WAL: %v", err)
	}
}

func TestFailedWALRequiresRecovery(t *testing.T) {
	const offset = uint64(1_000)
	dir := t.TempDir()
	idx, err := NewStore(dir, Spec{Offset: offset, Entries: []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}}, Config{BlockSize: 2, DeltaCapacity: 8}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	ok := NewBatch(1)
	ok.Append(Insert(2, offset+2*testQuantum))
	if err := idx.Apply(ok); err != nil {
		t.Fatal(err)
	}
	before := idx.Search(nil, offset, offset+10*testQuantum)
	// Fault: the next WAL append fails; the deltas must not advance.
	breakWAL(t, idx)
	failed := NewBatch(1)
	failed.Append(Insert(3, offset+3*testQuantum))
	err = idx.Apply(failed)
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Apply err = %v, want ErrRecoveryRequired", err)
	}
	// Latched: repeats fail identically, the view is untouched, and
	// maintenance refuses instead of papering over the gap.
	if err := idx.Apply(failed); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("second Apply err = %v, want ErrRecoveryRequired", err)
	}
	if err := idx.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Checkpoint err = %v, want ErrRecoveryRequired", err)
	}
	if err := idx.Vacuum(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Vacuum err = %v, want ErrRecoveryRequired", err)
	}
	if got := idx.Search(nil, offset, offset+10*testQuantum); !equalIDSet(got, before) {
		t.Fatalf("failed write advanced view: %v -> %v", before, got)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Recovery is close plus reopen: the failed append never touched the
	// file (it failed seeking before writing), so the tail replays clean
	// and the failed batch is absent.
	reopened, err := OpenStore(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Search(nil, offset, offset+10*testQuantum); !equalIDSet(got, before) {
		t.Fatalf("reopen search = %v, want %v", got, before)
	}
}
