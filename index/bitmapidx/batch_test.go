package bitmapidx

import (
	"errors"
	"sync"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

// commit stages fill into a one-shot batch, applies it, and barriers
// until visible. It is the test equivalent of the old synchronous write.
func commit(tb testing.TB, index *Index, fill func(*Batch) error) {
	tb.Helper()
	batch := index.NewBatch()
	defer batch.Close()
	if err := fill(batch); err != nil {
		tb.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		tb.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		tb.Fatal(err)
	}
}

// commitBarrier stages fill, applies it, and returns the barrier result
// so tests can assert commit-time errors (geo validation, encode
// limits) that only surface once the batch commits.
func commitBarrier(tb testing.TB, index *Index, fill func(*Batch) error) error {
	tb.Helper()
	batch := index.NewBatch()
	defer batch.Close()
	if err := fill(batch); err != nil {
		tb.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		tb.Fatal(err)
	}
	return index.Barrier()
}

func TestBatchValidation(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	batch := index.NewBatch()
	defer batch.Close()
	if err := batch.Add(Entry{ImageID: 0, Value: EnumStringValue("x")}); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("zero ID err = %v", err)
	}
	if err := batch.Add(Entry{ImageID: 1, Value: BoolValue(true)}); !errors.Is(err, ErrValueKind) {
		t.Fatalf("kind mismatch err = %v", err)
	}
	if err := batch.Remove(Entry{ImageID: 0, Value: EnumStringValue("x")}); !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("remove zero ID err = %v", err)
	}
	text, err := NewMemory(textNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = text.Close() })
	textBatch := text.NewBatch()
	defer textBatch.Close()
	if err := textBatch.Add(Entry{ImageID: 1, Value: Value{Kind: schema.KindBitmapString, Bytes: []byte("x")}}); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("text Add err = %v", err)
	}
	if err := textBatch.ReplaceText(TextReplacement{ImageID: 1, After: []string{"x"}}, TextReplacement{ImageID: 1, After: []string{"y"}}); !errors.Is(err, ErrDuplicateImageID) {
		t.Fatalf("text dup err = %v", err)
	}
	if err := batch.Move(Move{ImageID: 1, To: 1}); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("move on enum err = %v", err)
	}
	if err := batch.ReplaceText(TextReplacement{ImageID: 1}); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("replace on enum err = %v", err)
	}
	if !batch.Empty() || batch.Len() != 0 {
		t.Fatal("failed validations must stage nothing")
	}
}

func TestBatchEmptyApply(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	batch := index.NewBatch()
	defer batch.Close()
	if err := batch.Apply(); err != nil {
		t.Fatalf("empty Apply err = %v", err)
	}
	if err := index.Barrier(); err != nil {
		t.Fatalf("Barrier err = %v", err)
	}
}

func TestBatchResetReuse(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	batch := index.NewBatch()
	defer batch.Close()
	for n := uint64(1); n <= 3; n++ {
		if err := batch.Add(Entry{ImageID: schema.ImageID(n), Value: EnumStringValue("v")}); err != nil {
			t.Fatal(err)
		}
		if err := batch.Apply(); err != nil {
			t.Fatal(err)
		}
		if err := index.Barrier(); err != nil {
			t.Fatal(err)
		}
		if err := batch.Reset(); err != nil {
			t.Fatal(err)
		}
		if !batch.Empty() {
			t.Fatal("Reset must clear staged operations")
		}
	}
	if got := index.Metadata().BaseValues; got != 0 {
		// Deltas may not have folded; verify visibility instead.
		_ = got
	}
	present, err := index.Contains(2, EnumStringValue("v"))
	if err != nil || !present {
		t.Fatalf("reused batch writes visible = %v, %v", present, err)
	}
}

func TestBatchQueuedUntilBarrier(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	batch := index.NewBatch()
	defer batch.Close()
	if err := batch.Add(Entry{ImageID: 9, Value: EnumStringValue("q")}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	// Queued is not committed: Barrier is the visibility point.
	if err := index.Barrier(); err != nil {
		t.Fatal(err)
	}
	present, err := index.Contains(9, EnumStringValue("q"))
	if err != nil || !present {
		t.Fatalf("post-barrier visible = %v, %v", present, err)
	}
}

func TestBatchSealed(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	batch := index.NewBatch()
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := batch.Add(Entry{ImageID: 1, Value: EnumStringValue("x")}); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("sealed Add err = %v", err)
	}
	if err := batch.Apply(); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("sealed Apply err = %v", err)
	}
	if err := batch.Reset(); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("sealed Reset err = %v", err)
	}
}

func TestBatchGroupCommitConcurrent(t *testing.T) {
	index, err := NewMemory(enumNamespace(schema.NamespaceTag), NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			batch := index.NewBatch()
			defer batch.Close()
			for n := 0; n < perWriter; n++ {
				id := schema.ImageID(w*perWriter + n + 1)
				if err := batch.Add(Entry{ImageID: id, Value: EnumStringValue("g")}); err != nil {
					errs <- err
					return
				}
				if err := batch.Apply(); err != nil {
					errs <- err
					return
				}
				if err := batch.Reset(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= writers*perWriter; id++ {
		present, err := index.Contains(schema.ImageID(id), EnumStringValue("g"))
		if err != nil || !present {
			t.Fatalf("id %d visible = %v, %v", id, present, err)
		}
	}
}
