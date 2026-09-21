package timeidx

import (
	"errors"
	"testing"
)

// TestStoreDirectoryOwnership proves single-writer exclusion: a second
// opener fails fast while the first holds the lock, and creating over a
// populated directory fails after the lock releases.
func TestStoreDirectoryOwnership(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{Offset: 1_000}
	first, err := NewPersistent(dir, spec, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistent(dir, spec, Config{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second NewPersistent err = %v, want ErrStoreBusy", err)
	}
	if _, err := OpenPersistent(dir, Config{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("OpenPersistent while locked err = %v, want ErrStoreBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistent(dir, spec, Config{}, DurabilityOS); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("NewPersistent over store err = %v, want ErrStoreExists", err)
	}
	reopened, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
