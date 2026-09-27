package hashidx

import (
	"errors"
	common "github.com/evanoberholster/imageIdx/common"
	"testing"
	"time"
)

func testWriterIndex(t *testing.T, dir string) *Index {
	t.Helper()
	idx, err := newIndex(dir, testPerceptualSpec(Config{}), common.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Threshold flushes would deadlock the pre-recovery worker, which called
// Flush while holding the buffer lock.
func TestBufferedWriterThresholdFlush(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 4, time.Hour)
	defer func() { _ = bw.Close() }()
	for i := 1; i <= 4; i++ {
		if err := bw.Apply([]Entry{{ImageID: common.ImageID(i), PHash64: uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "threshold flush", func() bool {
		f, e := bw.Stats()
		return f >= 1 && e >= 4
	})
	if got := idx.Metadata().RecordCount; got != 4 {
		t.Fatalf("RecordCount = %d, want 4", got)
	}
}

func TestBufferedWriterTickFlush(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 1<<20, 10*time.Millisecond)
	defer func() { _ = bw.Close() }()
	if err := bw.Apply([]Entry{{ImageID: 1, PHash64: 9}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tick flush", func() bool {
		f, _ := bw.Stats()
		return f >= 1
	})
	if got := idx.Metadata().RecordCount; got != 1 {
		t.Fatalf("RecordCount = %d, want 1", got)
	}
}

func TestBufferedWriterExplicitFlushAndStats(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 1<<20, time.Hour)
	defer func() { _ = bw.Close() }()
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []common.ImageID{1, 2, 3} {
		if err := bw.Apply([]Entry{{ImageID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	// Let the worker buffer everything first: the assertion below is
	// about single-publish batching, not about winning a race with the
	// worker's channel takes.
	waitFor(t, "buffering", func() bool {
		bw.mu.Lock()
		defer bw.mu.Unlock()
		return bw.pending == 3
	})
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if f, e := bw.Stats(); f != 1 || e != 3 {
		t.Fatalf("Stats = (%d, %d), want (1, 3)", f, e)
	}
	if got := idx.Metadata().RecordCount; got != 3 {
		t.Fatalf("RecordCount = %d, want 3", got)
	}
}

func TestBufferedWriterApplyFull(t *testing.T) {
	// No worker drains this writer, so the second send must fail fast
	// instead of blocking.
	bw := &BufferedWriter{ch: make(chan []Entry, 1)}
	if err := bw.Apply([]Entry{{ImageID: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := bw.Apply([]Entry{{ImageID: 2}}); !errors.Is(err, ErrChannelFull) {
		t.Fatalf("Apply err = %v, want ErrChannelFull", err)
	}
}

func TestBufferedWriterFailedFlushRetainsEntries(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 1<<20, time.Hour)
	defer func() { _ = bw.Close() }()
	if err := bw.Apply([]Entry{{ImageID: 1}, {ImageID: 2}}); err != nil {
		t.Fatal(err)
	}
	// Let the worker move the slice before breaking the index; no timer
	// or threshold flush can fire first.
	waitFor(t, "buffering", func() bool {
		bw.mu.Lock()
		defer bw.mu.Unlock()
		return bw.pending == 2
	})
	// Break the index so publish fails: entries must survive in the
	// buffer ahead of anything queued later.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush err = %v, want ErrClosed", err)
	}
	bw.mu.Lock()
	pending := bw.pending
	bw.mu.Unlock()
	if pending != 2 {
		t.Fatalf("retained pending = %d, want 2", pending)
	}
	if f, _ := bw.Stats(); f != 0 {
		t.Fatalf("flushes = %d, want 0", f)
	}
}

func TestBufferedWriterCloseDrainsAndIdempotent(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 1<<20, time.Hour)
	for i := 1; i <= 5; i++ {
		if err := bw.Apply([]Entry{{ImageID: common.ImageID(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("second Close err = %v, want nil", err)
	}
	if got := idx.Metadata().RecordCount; got != 5 {
		t.Fatalf("RecordCount = %d, want 5", got)
	}
}

func TestBufferedWriterConcurrentApply(t *testing.T) {
	idx := testWriterIndex(t, t.TempDir())
	bw := NewBufferedWriter(idx, 64, 5*time.Millisecond)
	defer func() { _ = bw.Close() }()
	const writers = 4
	const perWriter = 50
	done := make(chan struct{})
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer func() {
				if w == 0 {
					close(done)
				}
			}()
			for k := 0; k < perWriter; k++ {
				id := common.ImageID(1 + w*perWriter + k)
				for {
					if err := bw.Apply([]Entry{{ImageID: id}}); err == nil {
						break
					} else if !errors.Is(err, ErrChannelFull) {
						t.Errorf("Apply err = %v", err)
						return
					}
				}
			}
		}(w)
	}
	<-done
	waitFor(t, "drain", func() bool {
		_, e := bw.Stats()
		return e >= writers*perWriter
	})
}
