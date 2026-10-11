package geoidx

import (
	common "github.com/evanoberholster/imageIdx/common"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
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

// TestOpenRejectsMalformed checks that a damaged superblock, shard extent,
// directory, or truncated file yields an error rather than a panic.
func TestOpenRejectsMalformed(t *testing.T) {
	build := func(t *testing.T) (dir string, config Config, shardOffset, dirOffset uint64) {
		t.Helper()
		dir = t.TempDir()
		config = Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
		index, err := New(dir, config)
		if err != nil {
			t.Fatal(err)
		}
		insertKeys(t, index, validKeys(t, 50), 1)
		if err := index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if shards := index.view.Load().shards; len(shards) > 0 {
			shardOffset = shards[0].offset
		}
		dirOffset = index.dirOffset
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		return dir, config, shardOffset, dirOffset
	}
	corrupt := func(t *testing.T, dir string, offset uint64) {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, geoidxFile), os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var b [1]byte
		if _, err := f.ReadAt(b[:], int64(offset)); err != nil {
			t.Fatal(err)
		}
		b[0] ^= 0xFF
		if _, err := f.WriteAt(b[:], int64(offset)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("superblock", func(t *testing.T) {
		dir, config, _, _ := build(t)
		corrupt(t, dir, 0)               // slot 0
		corrupt(t, dir, superblockBytes) // slot 1: no valid generation remains
		if _, err := Open(dir, config); err == nil {
			t.Fatal("Open accepted a corrupt superblock")
		}
	})
	t.Run("shard-falls-back", func(t *testing.T) {
		dir, config, shardOffset, _ := build(t)
		corrupt(t, dir, shardOffset)
		index, err := Open(dir, config)
		if err != nil {
			t.Fatalf("Open should fall back to the previous generation: %v", err)
		}
		defer index.Close()
		if !validateDirectory(index.view.Load().shards) {
			t.Fatal("fallback directory invalid")
		}
		// The corrupt newest generation must be rejected: the fallback here is the
		// empty generation created before the Checkpoint.
		if got := index.Len(); got != 0 {
			t.Fatalf("corrupt newest generation not rejected: Len = %d, want 0", got)
		}
	})
	t.Run("directory-falls-back", func(t *testing.T) {
		dir, config, _, dirOffset := build(t)
		corrupt(t, dir, dirOffset+16) // the first entry's offset field
		index, err := Open(dir, config)
		if err != nil {
			t.Fatalf("Open should fall back to the previous generation: %v", err)
		}
		defer index.Close()
		if !validateDirectory(index.view.Load().shards) {
			t.Fatal("fallback directory invalid")
		}
		if got := index.Len(); got != 0 {
			t.Fatalf("corrupt newest generation not rejected: Len = %d, want 0", got)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		dir, config, _, _ := build(t)
		if err := os.Truncate(filepath.Join(dir, geoidxFile), 64); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, config); err == nil {
			t.Fatal("Open accepted a truncated file")
		}
	})
}

// TestSortIDsDifferential checks the bitset, radix, and comparison sorts against
// sort+compact over dense, sparse, duplicate-heavy, empty, and single domains.
func TestSortIDsDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	cases := []struct {
		name  string
		n     int
		maxID common.ImageID
	}{
		{"empty", 0, 1000},
		{"single", 1, 1000},
		{"small", 100, 1_000_000_000_000},
		{"dense", 50_000, 60_000},
		{"dense-duplicate-heavy", 50_000, 1_000},
		{"sparse-bitset-rejected", 50_000, 1 << 40},
		{"sparse-large", 20_000, 1 << 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]common.ImageID, tc.n)
			for i := range ids {
				ids[i] = common.ImageID(uint64(rng.Int63n(int64(tc.maxID))) + 1)
			}
			want := append([]common.ImageID(nil), ids...)
			slices.Sort(want)
			want = slices.Compact(want)
			got := sortIDs(append([]common.ImageID(nil), ids...), tc.maxID)
			if !slices.Equal(got, want) {
				t.Fatalf("sortIDs: got %d ids, want %d", len(got), len(want))
			}
		})
	}
}

// TestSortSuffixPreservesPrefix verifies only the appended suffix is sorted and
// that duplicates are removed without an extra allocation when already sorted.
func TestSortSuffixPreservesPrefix(t *testing.T) {
	dst := []common.ImageID{9, 9, 5, 3, 5, 1}
	got := sortSuffix(dst, 2, 10)
	want := []common.ImageID{9, 9, 1, 3, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("sortSuffix = %v, want %v", got, want)
	}
	// Already-ordered suffix: dedup only, prefix untouched.
	ordered := []common.ImageID{7, 7, 2, 2, 3, 3}
	got = sortSuffix(ordered, 2, 10)
	if want := []common.ImageID{7, 7, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("sorted suffix = %v, want %v", got, want)
	}
	// Empty suffix leaves dst unchanged.
	empty := []common.ImageID{4, 4}
	if got := sortSuffix(empty, 2, 10); !slices.Equal(got, empty) {
		t.Fatalf("empty suffix = %v", got)
	}
}

// TestPersistentSize records the live payload, arena capacity, and single-file
// layout for a fixed image count so storage changes are visible. Run with -v.
func TestPersistentSize(t *testing.T) {
	if testing.Short() {
		t.Skip("persistent size check")
	}
	const n = 1_000_000
	dir := t.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]Entry, n)
	for i := range entries {
		lat := testLat + float64(i%1000)*0.0004
		lng := testLng + float64(i/1000%1000)*0.0004
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: common.ImageID(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		t.Fatal(err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	stats := index.StorageStats()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 2 { // geoidx.dat + geoidx.lock
		t.Fatalf("persistent directory has %d files, want 2: %v", len(files), files)
	}
	info, err := os.Stat(filepath.Join(dir, geoidxFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("geoidx persistent 1M: file=%d arena=%d live=%d free=%d largestFree=%d runs=%d (%d B/image)",
		info.Size(), stats.ArenaBytes, stats.LiveBytes, stats.FreeBytes, stats.LargestFree, stats.FreeRuns, stats.LiveBytes/uint64(n))
}
