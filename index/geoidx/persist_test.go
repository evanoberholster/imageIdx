package geoidx

import (
	"os"
	"path/filepath"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

func validKeys(t *testing.T, n int) []S2Key {
	t.Helper()
	keys := testKeys(n)
	// testKeys may contain duplicates; the base requires strict (key,id) order.
	valid := make([]S2Key, 0, n)
	var prev S2Key
	for _, key := range keys {
		if key <= prev {
			continue
		}
		valid = append(valid, key)
		prev = key
	}
	return valid
}

func TestValidateBase(t *testing.T) {
	keys := validKeys(t, 100)
	ids := make([]common.ImageID, len(keys))
	var max common.ImageID
	for i := range ids {
		ids[i] = common.ImageID(i + 1)
		max = ids[i]
	}
	if err := validateBase(keys, ids, max); err != nil {
		t.Fatalf("valid base rejected: %v", err)
	}
	if err := validateBase(keys, ids[:len(ids)-1], max); err != ErrCorrupt {
		t.Fatalf("length mismatch = %v, want ErrCorrupt", err)
	}

	zeroID := append([]common.ImageID(nil), ids...)
	zeroID[5] = 0
	if err := validateBase(keys, zeroID, max); err != ErrCorrupt {
		t.Fatalf("zero id = %v, want ErrCorrupt", err)
	}

	badKey := append([]S2Key(nil), keys...)
	badKey[5] = 0
	if err := validateBase(badKey, ids, max); err != ErrCorrupt {
		t.Fatalf("invalid cell = %v, want ErrCorrupt", err)
	}

	unsorted := append([]S2Key(nil), keys...)
	unsorted[3], unsorted[4] = unsorted[4], unsorted[3]
	if err := validateBase(unsorted, ids, max); err != ErrCorrupt {
		t.Fatalf("unsorted keys = %v, want ErrCorrupt", err)
	}

	if err := validateBase(keys, ids, max+1); err != ErrCorrupt {
		t.Fatalf("maxID mismatch = %v, want ErrCorrupt", err)
	}
	if err := validateBase(nil, nil, 0); err != nil {
		t.Fatalf("empty base rejected: %v", err)
	}
}

// TestOpenRejectsMalformed checks that a damaged manifest or base file yields
// ErrCorrupt rather than a panic.
func TestOpenRejectsMalformed(t *testing.T) {
	build := func(t *testing.T) (string, Config) {
		t.Helper()
		dir := t.TempDir()
		config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
		index, err := New(dir, config)
		if err != nil {
			t.Fatal(err)
		}
		insertKeys(t, index, validKeys(t, 50), 1)
		if err := index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		return dir, config
	}

	t.Run("manifest-crc", func(t *testing.T) {
		dir, config := build(t)
		data, err := os.ReadFile(filepath.Join(dir, "manifest"))
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)-1] ^= 0xFF
		if err := os.WriteFile(filepath.Join(dir, "manifest"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, config); err == nil {
			t.Fatal("Open accepted a corrupt manifest")
		}
	})

	t.Run("base-truncated", func(t *testing.T) {
		dir, config := build(t)
		manifest, err := os.ReadFile(filepath.Join(dir, "manifest"))
		if err != nil {
			t.Fatal(err)
		}
		generation, _, ok := decodeManifest(manifest)
		if !ok {
			t.Fatal("decode manifest")
		}
		path := basePath(dir, generation)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, info.Size()/2); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, config); err == nil {
			t.Fatal("Open accepted a truncated base")
		}
	})

	t.Run("base-magic", func(t *testing.T) {
		dir, config := build(t)
		manifest, _ := os.ReadFile(filepath.Join(dir, "manifest"))
		generation, _, _ := decodeManifest(manifest)
		path := basePath(dir, generation)
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt([]byte{0, 0, 0, 0}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, config); err == nil {
			t.Fatal("Open accepted a base with a bad magic")
		}
	})
}
