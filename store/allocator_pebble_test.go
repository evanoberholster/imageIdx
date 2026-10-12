package store

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/idalloc"
)

func openTestDB(t *testing.T, dir string) *pebble.DB {
	t.Helper()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func wrapTestDB(db *pebble.DB) *Store { return &Store{db: db} }

func TestPebbleReopenAndMigrationWatermarks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	db := openTestDB(t, dir)
	values := wrapTestDB(db)
	if _, err := values.OpenReservations(); !errors.Is(err, ErrUninitializedReservations) {
		t.Fatalf("uninitialized: %v", err)
	}
	// These maxima stand for an explicit canonical + tombstone migration scan.
	var maxima [common.EntityKindCount + 1]uint64
	maxima[common.EntityImage] = 4234
	if err := values.InitializeReservations(maxima); err != nil {
		t.Fatal(err)
	}
	if err := values.InitializeReservations(maxima); err == nil {
		t.Fatal("reinitialized metadata")
	}
	s, err := values.OpenReservations()
	if err != nil {
		t.Fatal(err)
	}
	a, err := idalloc.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := a.Next(common.EntityImage); id != 4235 || err != nil {
		t.Fatalf("%d, %v", id, err)
	}
	if id, err := a.Next(common.EntityPerson); id != 1 || err != nil {
		t.Fatalf("%d, %v", id, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestDB(t, dir)
	values = wrapTestDB(db)
	s, err = values.OpenReservations()
	if err != nil {
		t.Fatal(err)
	}
	a, err = idalloc.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := a.Next(common.EntityImage); id != 5235 || err != nil {
		t.Fatalf("reopen: %d, %v", id, err)
	}
	if err := s.ReserveThrough(common.EntityImage, 1); err == nil {
		t.Fatal("lowered watermark")
	}
	_ = a.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPebbleCorruptMetadataFailsClosed(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	values := wrapTestDB(db)
	if err := values.InitializeReservations([common.EntityKindCount + 1]uint64{}); err != nil {
		t.Fatal(err)
	}
	key := metadataKey(common.EntityImage)
	if err := db.Set(key[:], []byte("bad"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := values.OpenReservations(); err == nil {
		t.Fatal("accepted corrupt metadata")
	}
	if err := db.Delete(key[:], pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := values.OpenReservations(); err == nil {
		t.Fatal("accepted missing metadata")
	}
}

func TestMissingAllocatorMarkerDoesNotTriggerMigration(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	values := wrapTestDB(db)
	if err := values.InitializeReservations([common.EntityKindCount + 1]uint64{}); err != nil {
		t.Fatal(err)
	}
	marker := metadataKey(common.EntityInvalid)
	if err := db.Delete(marker[:], pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := values.OpenReservations(); err == nil || errors.Is(err, ErrUninitializedReservations) {
		t.Fatalf("missing marker must fail closed: %v", err)
	}
	if err := values.InitializeReservations([common.EntityKindCount + 1]uint64{}); err == nil {
		t.Fatal("reinitialized missing marker")
	}
}

func TestAllocatorMetadataOutsideHighestUserNamespace(t *testing.T) {
	s, err := Open(t.TempDir(), Spec{Namespace: 65535, Name: "highest"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitializeReservations([common.EntityKindCount + 1]uint64{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Key{Namespace: 65535, ID: 7}, []byte("record")); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := s.Scan(65535, func(key Key, _ []byte) error {
		count++
		if key.ID != 7 {
			t.Fatalf("unexpected user key: %v", key)
		}
		return nil
	}); err != nil || count != 1 {
		t.Fatalf("highest namespace scan: count=%d, err=%v", count, err)
	}
}

func TestUpgradeReservationsPreservesOldWatermarks(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	marker := metadataKey(common.EntityInvalid)
	batch := db.NewBatch()
	for n := 1; n <= legacyEntityCount; n++ {
		key := metadataKey(common.EntityKind(n))
		v := encodeWatermark(common.EntityKind(n), 5000)
		v[0] = legacyMetadataVersion
		binary.BigEndian.PutUint32(v[10:], crc32.ChecksumIEEE(v[:10]))
		if err := batch.Set(key[:], v[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Set(marker[:], []byte{legacyMetadataVersion, legacyEntityCount}, nil); err != nil {
		t.Fatal(err)
	}
	if err := batch.Set(initializationWitness[:], []byte{legacyMetadataVersion}, nil); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	_ = batch.Close()
	s := wrapTestDB(db)
	if _, err := s.OpenReservations(); !errors.Is(err, ErrLegacyReservations) {
		t.Fatalf("old metadata: %v", err)
	}
	var maxima [common.EntityKindCount + 1]uint64
	maxima[common.EntityImage] = 5100 // legacy canonical record beyond watermark
	maxima[common.EntityUser] = 7000
	if err := s.UpgradeReservations(maxima); err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenReservations()
	if err != nil {
		t.Fatal(err)
	}
	a, err := idalloc.Open(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind common.EntityKind
		want uint64
	}{
		{common.EntityImage, 5101}, {common.EntityFace, 5001}, {common.EntityUser, 7001}, {common.EntityLocation, 1}, {common.EntityTag, 1},
	} {
		id, err := a.Next(tc.kind)
		if err != nil || uint64(id) != tc.want {
			t.Fatalf("%s = %d, %v (want %d)", tc.kind, id, err, tc.want)
		}
	}
	_ = a.Close()
}

func TestPebbleNearOverflow(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "db"))
	defer db.Close()
	var maxima [common.EntityKindCount + 1]uint64
	maxima[common.EntityStack] = math.MaxUint64 - 1
	values := wrapTestDB(db)
	if err := values.InitializeReservations(maxima); err != nil {
		t.Fatal(err)
	}
	s, err := values.OpenReservations()
	if err != nil {
		t.Fatal(err)
	}
	a, err := idalloc.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := a.Next(common.EntityStack); uint64(id) != math.MaxUint64 || err != nil {
		t.Fatalf("%d, %v", id, err)
	}
	if _, err := a.Next(common.EntityStack); !errors.Is(err, idalloc.ErrExhausted) {
		t.Fatal(err)
	}
	_ = a.Close()
}

func TestAllocatorCrashAfterIssuance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	if os.Getenv("IDALLOC_CRASH_CHILD") == "1" {
		dir = os.Getenv("IDALLOC_CRASH_DIR")
		db := openTestDB(t, dir)
		values := wrapTestDB(db)
		if err := values.InitializeReservations([common.EntityKindCount + 1]uint64{}); err != nil {
			t.Fatal(err)
		}
		s, err := values.OpenReservations()
		if err != nil {
			t.Fatal(err)
		}
		a, err := idalloc.Open(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Next(common.EntityImage); err != nil {
			t.Fatal(err)
		}
		// Simulate process death before record commit, allocator Close, or DB Close.
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAllocatorCrashAfterIssuance$")
	cmd.Env = append(os.Environ(), "IDALLOC_CRASH_CHILD=1", "IDALLOC_CRASH_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	db := openTestDB(t, dir)
	defer db.Close()
	s, err := wrapTestDB(db).OpenReservations()
	if err != nil {
		t.Fatal(err)
	}
	a, err := idalloc.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := a.Next(common.EntityImage); id != 1001 || err != nil {
		t.Fatalf("after crash: %d, %v", id, err)
	}
	_ = a.Close()
}

func BenchmarkReservationPebbleSync(b *testing.B) {
	s, err := Open(b.TempDir(), Spec{Namespace: 1, Name: "entity-image"})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if err := s.InitializeReservations([common.EntityKindCount + 1]uint64{}); err != nil {
		b.Fatal(err)
	}
	reservations, err := s.OpenReservations()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := reservations.ReserveThrough(common.EntityImage, uint64(i+1)*idalloc.BlockSize); err != nil {
			b.Fatal(err)
		}
	}
}
