package timeidx

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func persistent(t testing.TB, cfg Config) *Index {
	t.Helper()
	i, err := NewStore(t.TempDir(), Spec{Namespace: 1}, cfg, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { i.Close() })
	return i
}

func TestPersistentReopenSelectiveCompactionAndReuse(t *testing.T) {
	i := persistent(t, Config{MaxShardBytes: 512, MaxFileBytes: 4096})
	b := NewBatch(100)
	for n := uint64(0); n < 100; n++ {
		b.Append(Insert(n, n))
	}
	if err := i.Apply(b); err != nil {
		t.Fatal(err)
	}
	before := i.view.Load()
	affected := shardFor(before.shards, 50, 50)
	offsets := make([]uint64, len(before.shards))
	for n, s := range before.shards {
		offsets[n] = s.offset
	}
	apply(t, i, Move(50, 50, 51))
	if i.CommitStats().RewrittenShards != 1 {
		t.Fatal("unrelated rewrite", i.CommitStats())
	}
	for n, s := range i.view.Load().shards {
		if n != affected && s.offset != offsets[n] {
			t.Fatal("unrelated shard moved")
		}
	}
	for n := 0; n < 20; n++ {
		apply(t, i, Move(50, 0, uint64(50+n%2)))
	}
	size := i.OnDiskBytes()
	for n := 0; n < 20; n++ {
		apply(t, i, Move(50, 0, uint64(50+n%2)))
	}
	if i.OnDiskBytes() != size {
		t.Fatal("unbounded file growth")
	}
	path := i.path
	dir := filepath.Dir(path)
	generation := i.Generation()
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := OpenNamespace(dir, 1, Config{MaxShardBytes: 512}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.Generation() != generation || j.Len() != 100 {
		t.Fatal(j.Metadata())
	}
	if err := VerifyNamespace(dir, 1); err != nil {
		t.Fatal(err)
	}
	apply(t, j, Delete(50, 0))
	if j.Len() != 99 {
		t.Fatal(j.Len())
	}
}

func TestCommitFailureStages(t *testing.T) {
	for _, stage := range []string{"allocate", "write", "directory", "data-sync", "commit", "partial-root", "root-sync"} {
		t.Run(stage, func(t *testing.T) {
			i := persistent(t, Config{MaxShardBytes: 256})
			apply(t, i, Insert(1, 1))
			oldGen := i.Generation()
			i.fault = func(s string) error {
				if s == stage {
					return errors.New("injected")
				}
				return nil
			}
			b := NewBatch(1)
			b.Append(Insert(2, 2))
			err := i.Apply(b)
			if err == nil || i.Generation() != oldGen || !slices.Equal(i.Search(nil, 0, 3), []uint64{1}) {
				t.Fatal("failed commit published", err)
			}
			i.fault = nil
			uncertain := stage == "partial-root" || stage == "root-sync"
			if uncertain {
				if !errors.Is(err, ErrRecoveryRequired) || !errors.Is(i.Apply(b), ErrRecoveryRequired) {
					t.Fatal("uncertainty not latched")
				}
			} else {
				if err := i.Apply(b); err != nil {
					t.Fatal("retry", err)
				}
			}
			dir := filepath.Dir(i.path)
			i.Close()
			j, err := OpenNamespace(dir, 1, Config{MaxShardBytes: 256}, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			if j.Len() < 1 || j.Len() > 2 {
				t.Fatal(j.Len())
			}
			apply(t, j, Insert(3, 3))
		})
	}
}

func TestGenerationFallbackAndCorruption(t *testing.T) {
	for _, kind := range []string{"root", "directory", "shard", "truncate", "unaligned-file", "version", "overlap", "duplicate-id", "bounds"} {
		t.Run(kind, func(t *testing.T) {
			i := persistent(t, Config{MaxShardBytes: 16})
			apply(t, i, Insert(1, 1), Insert(2, 2))
			path := i.path
			slot := i.slot
			i.Close()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			off := slot * 512
			h, p, ok := storage.DecodeSuperblock(data[off:off+512], fileMagic, fileVersion)
			if !ok {
				t.Fatal("slot")
			}
			root := binary.LittleEndian.Uint64(p[8:16])
			count := binary.LittleEndian.Uint32(p[16:20])
			dir := data[root : root+uint64(count)*directoryEntrySize]
			allBad := false
			switch kind {
			case "root":
				data[off+20] ^= 1
			case "directory":
				dir[0] ^= 1
			case "shard":
				data[binary.LittleEndian.Uint64(dir[16:24])] ^= 1
			case "truncate":
				data = data[:100]
				allBad = true
			case "unaligned-file":
				data = append(data, 0)
				allBad = true
			case "version":
				h.Version++
				storage.EncodeSuperblock(data[off:off+512], h, p)
				allBad = true
			case "overlap":
				copy(dir[directoryEntrySize+16:directoryEntrySize+24], dir[16:24])
				binary.LittleEndian.PutUint32(p[20:24], storage.Checksum(dir))
				storage.EncodeSuperblock(data[off:off+512], h, p)
			case "duplicate-id":
				e := dir[directoryEntrySize:]
				loc := binary.LittleEndian.Uint64(e[16:24])
				binary.LittleEndian.PutUint64(data[loc+8:loc+16], 1)
				binary.LittleEndian.PutUint64(e[8:16], 1)
				binary.LittleEndian.PutUint64(e[40:48], 1)
				binary.LittleEndian.PutUint64(e[48:56], 1)
				binary.LittleEndian.PutUint32(e[32:36], storage.Checksum(data[loc+8:loc+16]))
				binary.LittleEndian.PutUint32(p[20:24], storage.Checksum(dir))
				storage.EncodeSuperblock(data[off:off+512], h, p)
			case "bounds":
				binary.LittleEndian.PutUint64(p[8:16], ^uint64(0)-7)
				storage.EncodeSuperblock(data[off:off+512], h, p)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			j, err := OpenNamespace(filepath.Dir(path), 1, Config{}, DurabilitySync)
			if allBad {
				if err == nil {
					j.Close()
					t.Fatal("accepted malformed file")
				}
				return
			}
			if err != nil {
				t.Fatal("fallback", err)
			}
			defer j.Close()
			if j.Len() != 0 {
				t.Fatal("did not fall back", j.Len())
			}
		})
	}
}

func TestHistoricalPersistentDifferential(t *testing.T) {
	i := persistent(t, Config{MaxShardBytes: 1024})
	for _, size := range []int{1, 8, 32, 64} {
		b := NewBatch(size)
		for n := 0; n < size; n++ {
			b.Append(Insert(uint64(n), uint64(n)*100000000000000000))
		}
		if err := i.Apply(b); err != nil {
			t.Fatal(err)
		}
		b.Reset()
		for n := 0; n < size; n++ {
			if n%3 == 0 {
				b.Append(Delete(uint64(n), 0))
			} else {
				b.Append(Move(uint64(n), 0, uint64(63-n)*100000000000000000))
			}
		}
		if err := i.Apply(b); err != nil {
			t.Fatal(err)
		}
		want := i.Search(nil, 0, ^uint64(0))
		dir := filepath.Dir(i.path)
		i.Close()
		var err error
		i, err = OpenNamespace(dir, 1, Config{MaxShardBytes: 1024}, DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(i.Search(nil, 0, ^uint64(0)), want) {
			t.Fatal("recovery differential")
		}
	}
	i.Close()
}

func TestPersistentEqualTimesAndIndependentGrowth(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewNamespaceStore(dir, Config{MaxShardBytes: 512, MaxFileBytes: 4096}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	a, err := manager.CreateNamespace(1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.CreateNamespace(2)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, b, Insert(7, 42))
	bs := b.CommitStats()
	bg := b.Generation()
	bytes := b.OnDiskBytes()
	batch := NewBatch(100000)
	for n := uint64(0); n < 100000; n++ {
		batch.Append(Insert(n, 42))
	}
	if err := a.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if a.Metadata().BlockCount <= 1 || len(a.Search(nil, 42, 43)) != 100000 {
		t.Fatal("equal-time split")
	}
	if b.CommitStats() != bs || b.Generation() != bg || b.OnDiskBytes() != bytes || !slices.Equal(b.Search(nil, 42, 43), []uint64{7}) {
		t.Fatal("growth isolation")
	}
	apply(t, a, Move(7, 42, 43))
	if len(a.Search(nil, 42, 43)) != 99999 || !slices.Equal(a.Search(nil, 43, 44), []uint64{7}) {
		t.Fatal("move across equal-key shards")
	}
	if err := manager.CloseNamespace(1); err != nil {
		t.Fatal(err)
	}
	a, err = manager.OpenNamespace(1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Len() != 100000 || len(a.Search(nil, 42, 43)) != 99999 {
		t.Fatal("reopen equal-key shards")
	}
}

func TestPersistentRandomizedReferenceAndSparseIDs(t *testing.T) {
	i := persistent(t, Config{MaxShardBytes: 256})
	dir := filepath.Dir(i.path)
	reference := map[uint64]uint64{}
	rng := rand.New(rand.NewSource(64))
	for _, size := range []int{1, 8, 32, 64} {
		for round := 0; round < 10; round++ {
			b := NewBatch(size)
			for n := 0; n < size; n++ {
				id, ts := uint64(rng.Intn(100))*100000000000000000, uint64(rng.Intn(64))*100000000000000000
				switch rng.Intn(3) {
				case 0:
					b.Append(Delete(id, 0))
					delete(reference, id)
				case 1:
					b.Append(Insert(id, ts))
					reference[id] = ts
				default:
					b.Append(Move(id, 0, ts))
					reference[id] = ts
				}
			}
			if err := i.Apply(b); err != nil {
				t.Fatal(err)
			}
			i.Close()
			var err error
			i, err = OpenNamespace(dir, 1, Config{MaxShardBytes: 256}, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			for start := uint64(0); start < 6400000000000000000; start += 1600000000000000000 {
				want := referenceRange(reference, start, start+1600000000000000000)
				if got := i.Search(nil, start, start+1600000000000000000); !slices.Equal(got, want) || i.EstimateRange(start, start+1600000000000000000) != uint64(len(want)) {
					t.Fatalf("size %d round %d: %v != %v", size, round, got, want)
				}
			}
		}
	}
	apply(t, i, Insert(^uint64(0), 1))
	i.Close()
	var err error
	i, err = OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if !slices.Equal(i.Search(nil, 1, 2), []uint64{^uint64(0)}) {
		t.Fatal("sparse ID recovery")
	}
}
