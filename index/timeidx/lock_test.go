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
	first, err := NewStore(dir, spec, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, spec, Config{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second NewStore err = %v, want ErrStoreBusy", err)
	}
	if _, err := OpenStore(dir, Config{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("OpenStore while locked err = %v, want ErrStoreBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, spec, Config{}, DurabilityOS); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("NewStore over store err = %v, want ErrStoreExists", err)
	}
	reopened, err := OpenStore(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
