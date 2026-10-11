package index_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// TestRecoveryMarkerPersistsPartialFailure verifies a mid-batch failure across
// engines leaves a durable recovery marker, and reopening marks the affected
// namespaces rebuild-required.
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

// TestRecoveryMarkerClearedOnSuccess verifies a successful risky batch is made
// durable and its marker cleared.
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

// TestSingleNamespaceBatchAvoidsMarker verifies a single-namespace batch is
// atomic, so it does not write a recovery marker (liveness is not a separate
// unit).
func TestSingleNamespaceBatchAvoidsMarker(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := db.NewBatch(3)
	for i := 1; i <= 3; i++ {
		if err := batch.Add(index.Entry{ValueID: common.ValueID(i), Times: []index.TimeValue{{Namespace: 1, Timestamp: uint64(i)}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); !os.IsNotExist(err) {
		t.Fatalf("recovery marker written for a single-namespace batch: %v", err)
	}
	ti, _ := db.Time(1)
	if got := ti.Search(nil, 0, 10); len(got) != 3 {
		t.Fatalf("single-namespace records = %v, want 3", got)
	}
}

// TestMultiNamespaceRiskyBatchClearsMarker verifies a batch spanning a durable
// (time) and a bitmap namespace is risky but succeeds, so the marker is written
// and then cleared.
func TestMultiNamespaceRiskyBatchClearsMarker(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := db.NewBatch(2)
	if err := batch.Add(index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Add(index.Entry{ValueID: 2, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("x")}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); !os.IsNotExist(err) {
		t.Fatalf("recovery marker not cleared after a successful risky batch: %v", err)
	}
	if _, ok := db.Time(1); !ok {
		t.Fatal("time namespace missing")
	}
	if tags, ok := db.Bitmap(6); !ok {
		t.Fatal("bitmap namespace missing")
	} else if bm, _ := tags.MatchValues(bitmapidx.EnumStringValue("x")); !bm.Contains(2) {
		t.Fatal("bitmap value missing")
	}
}

// TestManifestCorruptionRejected verifies malformed manifests are rejected.
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

// TestManifestCountMismatch increments the namespace count field to test a
// structural mismatch with a valid CRC.
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

// TestApplyWithGenerationDurableWithoutCheckpoint verifies ApplyWithGeneration
// establishes a durability barrier, so engine data survives a close/reopen
// without an explicit Checkpoint.
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
