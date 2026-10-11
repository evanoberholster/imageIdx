package index_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// P0-1: once Delete returns, the matcher must not return the deleted ID.
func TestMatcherDeleteVisibleImmediately(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		t.Fatal(err)
	}
	h := radixhash.Hash{0x33}
	for i := 1; i <= 16; i++ {
		if err := db.Add(index.Entry{ValueID: common.ValueID(i), Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 16; i++ {
		if err := db.Delete(common.EntityImage, common.ValueID(i)); err != nil {
			t.Fatal(err)
		}
		got, err := m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(matchIDs(got), common.ValueID(i)) {
			t.Fatalf("deleted id %d still matched after Delete returned: %v", i, got)
		}
	}
}

// P0-2: a mid-batch failure across engines leaves a durable recovery marker,
// and reopening marks the affected namespaces rebuild-required.
func TestRecoveryMarkerPersistsPartialFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	hashIdx, ok := db.Hash(4)
	if !ok {
		t.Fatal("hash namespace missing")
	}
	if err := hashIdx.Close(); err != nil {
		t.Fatal(err)
	}
	batch := db.NewBatch(2)
	if err := batch.Add(index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 5}}, Hashes: []index.HashValue{{Namespace: 4, Hash: radixhash.Hash{1}}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Apply(batch); err == nil {
		t.Fatal("expected a mid-batch failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); err != nil {
		t.Fatalf("recovery marker missing after partial failure: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Time(1); ok {
		t.Fatal("time namespace should be recovery-required")
	}
	if _, ok := reopened.Hash(4); ok {
		t.Fatal("hash namespace should be recovery-required")
	}
	if _, ok := reopened.Bitmap(6); !ok {
		t.Fatal("unrelated namespace should remain available")
	}
	rec := reopened.RecoveryNamespaces()
	slices.Sort(rec)
	if !slices.Contains(rec, common.Namespace(1)) || !slices.Contains(rec, common.Namespace(4)) {
		t.Fatalf("RecoveryNamespaces = %v, want [1 4]", rec)
	}
	if h, _ := reopened.HealthOf(1); !h.NeedsRebuild {
		t.Fatalf("health = %+v, want NeedsRebuild", h)
	}
	if err := reopened.ClearRecovery(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); !os.IsNotExist(err) {
		t.Fatalf("recovery marker not cleared: %v", err)
	}
}

// P0-2: a successful risky batch clears the marker.
func TestRecoveryMarkerClearedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 5}}, Hashes: []index.HashValue{{Namespace: 4, Hash: radixhash.Hash{1}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); !os.IsNotExist(err) {
		t.Fatalf("marker present after a successful batch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.RecoveryNamespaces()) != 0 {
		t.Fatalf("reopened recovery namespaces = %v", reopened.RecoveryNamespaces())
	}
	if _, ok := reopened.Time(1); !ok {
		t.Fatal("time namespace unavailable after a clean batch")
	}
}

// P1-1: malformed manifests are rejected.
func TestManifestCorruptionRejected(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "store.bin"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated": append([]byte(nil), original[:len(original)-1]...),
		"extra":     append(append([]byte(nil), original...), 0),
		"bad_crc":   flip(original, len(original)-1),
		"bad_magic": flip(original, 0),
		"bad_ver":   flip(original, 4),
		"unk_flags": flip(original, 8),
		"bad_count": flip(original, 6),
	}
	for name, corrupt := range cases {
		if err := os.WriteFile(filepath.Join(dir, "store.bin"), corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := index.Open(dir); err == nil {
			t.Fatalf("%s: corrupt manifest accepted", name)
		}
	}
}

func flip(src []byte, i int) []byte {
	out := append([]byte(nil), src...)
	out[i] ^= 0xFF
	return out
}

// Increment a small counter field to test structural mismatches.
func TestManifestCountMismatch(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "store.bin"))
	if err != nil {
		t.Fatal(err)
	}
	count := binary.LittleEndian.Uint16(raw[6:8])
	binary.LittleEndian.PutUint16(raw[6:8], count+1)
	// Fix the CRC so only the structural mismatch is under test.
	body := raw[:len(raw)-4]
	binary.LittleEndian.PutUint32(raw[len(raw)-4:], storage.Checksum(body))
	if err := os.WriteFile(filepath.Join(dir, "store.bin"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Open(dir); err == nil {
		t.Fatal("manifest with an inflated namespace count accepted")
	}
}

// P0-1: a vector engine failure during flush must degrade the vector namespace
// and leave unrelated namespaces usable.
func TestVectorFlushFailureDegrades(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, ok := db.Vector(5)
	if !ok {
		t.Fatal("vector namespace missing")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 512)
	err = db.Add(index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}})
	if err == nil {
		t.Fatal("expected an error from the closed vector engine")
	}
	if _, ok := db.Vector(5); ok {
		t.Fatal("degraded vector namespace is still resolvable")
	}
	h, _ := db.HealthOf(5)
	if h.Available || !h.Stale {
		t.Fatalf("health = %+v, want unavailable+stale", h)
	}
	if _, ok := db.Bitmap(6); !ok {
		t.Fatal("unrelated namespace became unavailable")
	}
}

// P0-1: vector add/replace/delete through staging, persisted and reopened.
func TestVectorAddUpdateDeleteReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 512)
	vec[0] = 1
	if err := db.Add(index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}}); err != nil {
		t.Fatal(err)
	}
	v, _ := db.Vector(5)
	got, err := v.SearchFloat32(vec, nil, 1)
	if err != nil || len(got) != 1 || got[0].ValueID != 1 {
		t.Fatalf("initial search = %v, %v", got, err)
	}
	vec2 := make([]float32, 512)
	vec2[1] = 1
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}},
		After:  index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec2}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = v.SearchFloat32(vec2, nil, 1)
	if len(got) != 1 || got[0].ValueID != 1 {
		t.Fatalf("after update = %v", got)
	}
	if err := db.Delete(common.EntityImage, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = v.SearchFloat32(vec2, nil, 1)
	if len(got) != 0 {
		t.Fatalf("after delete = %v, want empty", got)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v2, ok := reopened.Vector(5)
	if !ok {
		t.Fatal("vector namespace missing after reopen")
	}
	got, _ = v2.SearchFloat32(vec2, nil, 1)
	if len(got) != 0 {
		t.Fatalf("reopened delete not persisted: %v", got)
	}
}

// P0-2: the no-removal fast path is safe across reopen because the persisted
// flag is only set when a removal is durably recorded.
func TestMatcherFastPathSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	h := radixhash.Hash{1}
	if err := db.Add(index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m, err := reopened.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{100}) {
		t.Fatalf("reopened fast-path matches = %v, want [100]", ids)
	}
}

// P0-3: ApplyWithGeneration establishes a durability barrier, so engine data
// survives a close/reopen without an explicit Checkpoint.
func TestApplyWithGenerationDurableWithoutCheckpoint(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	hA := radixhash.Hash{0xA}
	hB := radixhash.Hash{0xB}
	if err := db.Add(index.Entry{ValueID: 200, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}}); err != nil {
		t.Fatal(err)
	}
	batch := db.NewBatch(1)
	if err := batch.Add(index.Entry{ValueID: 201, Hashes: []index.HashValue{{Namespace: 4, Hash: hB}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyWithGeneration(batch, 5); err != nil {
		t.Fatal(err)
	}
	// No Checkpoint: the generation barrier must have made the touched engine's
	// data durable.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// The batch touched only namespace 4, so the store-wide safe watermark
	// (the minimum across namespaces) stays 0 even though namespace 4 advanced.
	if got := reopened.SourceGeneration(); got != 0 {
		t.Fatalf("SourceGeneration = %d, want 0 for a partial batch", got)
	}
	m, err := reopened.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		t.Fatal(err)
	}
	if ids := matchIDs(mustMatch(t, m, hA)); !slices.Equal(ids, []common.ValueID{200}) {
		t.Fatalf("hA after reopen = %v, want [200]", ids)
	}
	if ids := matchIDs(mustMatch(t, m, hB)); !slices.Equal(ids, []common.ValueID{201}) {
		t.Fatalf("hB after reopen = %v, want [201]", ids)
	}
}

func mustMatch(t *testing.T, m *index.Matcher, h radixhash.Hash) []index.Match {
	t.Helper()
	got, err := m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// P0-2: concurrent Add/Delete and Match must not race or return deleted IDs.
func TestMatcherConcurrentMutation(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Matcher(index.MatcherSpec{SHA256: 4, Perceptual: 3})
	if err != nil {
		t.Fatal(err)
	}
	h := radixhash.Hash{0x5A}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_, _ = m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
				q := uint64(1)
				_, _ = m.MatchOne(nil, index.MatchItem{PHash: &q}, index.MatchParams{PHashDistance: 1})
			}
		}()
	}
	for i := 0; i < 200; i++ {
		id := common.ValueID(i%32 + 1)
		if err := db.Add(index.Entry{ValueID: id, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := db.Delete(common.EntityImage, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
}
