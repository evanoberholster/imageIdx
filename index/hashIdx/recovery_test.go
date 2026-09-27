package hashidx

import (
	"errors"
	common "github.com/evanoberholster/imageIdx/common"
	"testing"
)

// breakWAL closes the index's WAL handle underneath it, so the next WAL
// append fails deterministically. White-box fault injection: it stands in
// for disk errors the test cannot otherwise produce portably.
func breakWAL(t *testing.T, close func() error) {
	t.Helper()
	if err := close(); err != nil {
		t.Fatalf("break WAL: %v", err)
	}
}

func TestPerceptualFailedWALRequiresRecovery(t *testing.T) {
	dir := t.TempDir()
	idx, err := newIndex(dir, testPerceptualSpec(Config{}), common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	first := benchEntries(16)
	if err := idx.Apply(first); err != nil {
		t.Fatal(err)
	}
	before := idx.Metadata().RecordCount
	// Fault: the next WAL append fails; the view must not advance.
	breakWAL(t, idx.persistence.wal.Close)
	failed := benchEntries(16)
	for k := range failed {
		failed[k].ImageID = common.ImageID(10_000 + k + 1)
	}
	err = idx.Apply(failed)
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Apply err = %v, want ErrRecoveryRequired", err)
	}
	// Latched: repeats fail identically, the view is untouched, and
	// maintenance refuses instead of papering over the gap.
	for _, op := range []func() error{
		func() error { return idx.Apply(failed) },
		func() error { return idx.Checkpoint() },
		func() error { return idx.Vacuum(nil) },
	} {
		if err := op(); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("latched op err = %v, want ErrRecoveryRequired", err)
		}
	}
	if got := idx.Metadata().RecordCount; got != before {
		t.Fatalf("failed write advanced view: %d -> %d", before, got)
	}
	// Reads keep serving the last durable state.
	if got := len(idx.SearchPHashRadius(nil, 0, 64, nil)); got != int(before) {
		t.Fatalf("search after failure = %d records, want %d", got, before)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Recovery is close plus reopen: the failed append never touched the
	// file (it failed seeking before writing), so the tail replays clean
	// and the failed batch is absent.
	reopened, err := openIndex(dir, Config{}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Metadata().RecordCount; got != before {
		t.Fatalf("reopen count = %d, want %d", got, before)
	}
}

func TestExactFailedWALRequiresRecovery(t *testing.T) {
	dir := t.TempDir()
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 512}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	h0 := benchHash(4242)
	if err := idx.Add(h0, 7); err != nil {
		t.Fatal(err)
	}
	// Fault while the writer loop is idle: deterministic failure point.
	breakWAL(t, func() error {
		idx.persistence.mu.Lock()
		defer idx.persistence.mu.Unlock()
		return idx.persistence.wal.Close()
	})
	h1 := benchHash(8484)
	err = idx.Add(h1, 8)
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Apply err = %v, want ErrRecoveryRequired", err)
	}
	// Fast-fail: the failed cycle above ran once, but latched waiters run
	// no further cycles.
	publishes, _ := idx.WriteStats()
	if err := idx.Add(h1, 8); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("second Apply err = %v, want ErrRecoveryRequired", err)
	}
	if got, _ := idx.WriteStats(); got != publishes {
		t.Fatalf("latched Apply published: %d -> %d", publishes, got)
	}
	for _, op := range []func() error{
		func() error { return idx.Checkpoint() },
		func() error { return idx.Vacuum() },
	} {
		if err := op(); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("latched maintenance err = %v, want ErrRecoveryRequired", err)
		}
	}
	if idx.Contains(h1) {
		t.Fatal("failed write is visible")
	}
	if !idx.Contains(h0) {
		t.Fatal("durable write lost after failure")
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{BlockSize: 512}, common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.Contains(h0) {
		t.Fatal("durable write lost across recovery")
	}
	if reopened.Contains(h1) {
		t.Fatal("failed write resurrected across recovery")
	}
	if got := reopened.LiveCount(); got != 1 {
		t.Fatalf("reopen LiveCount = %d, want 1", got)
	}
}
