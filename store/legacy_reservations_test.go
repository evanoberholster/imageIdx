package store

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestLegacyReservationsAndSchemaMigration(t *testing.T) {
	s, err := OpenLegacy(t.TempDir(), Spec{Namespace: 1, Name: "image"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, found, err := s.LegacyReservations(); found || err != nil {
		t.Fatalf("empty: %v %v", found, err)
	}
	if complete, err := s.SchemaMigrationComplete(); complete || err != nil {
		t.Fatal(err)
	}
	key := legacyKey(schema.EntityInvalid)
	if err := s.db.Set(key[:], []byte{2, byte(schema.EntityKindCount)}, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LegacyReservations(); err == nil {
		t.Fatal("accepted marker without witness")
	}
	if err := s.db.Set(legacyWitness[:], []byte{2}, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LegacyReservations(); err == nil {
		t.Fatal("accepted missing watermark")
	}
	for n := 1; n <= schema.EntityKindCount; n++ {
		key := legacyKey(schema.EntityKind(n))
		var v [14]byte
		v[0], v[1] = 2, byte(n)
		if n == 1 {
			binary.BigEndian.PutUint64(v[2:10], 5000)
		}
		binary.BigEndian.PutUint32(v[10:], crc32.ChecksumIEEE(v[:10]))
		if err := s.db.Set(key[:], v[:], pebble.Sync); err != nil {
			t.Fatal(err)
		}
	}
	high, found, err := s.LegacyReservations()
	if err != nil || !found || high[1] != 5000 {
		t.Fatalf("legacy high: %d, %v, %v", high[1], found, err)
	}
	key = legacyKey(schema.EntityImage)
	if err := s.db.Set(key[:], []byte("corrupt"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LegacyReservations(); err == nil {
		t.Fatal("accepted corrupt high-water")
	}
	if err := s.CompleteSchemaMigration(); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.SchemaMigrationComplete(); !complete || err != nil {
		t.Fatalf("migration: %v %v", complete, err)
	}
	if _, found, err := s.LegacyReservations(); found || err != nil {
		t.Fatalf("legacy keys not cleared: %v %v", found, err)
	}
}

func TestLegacyVersionOneReservations(t *testing.T) {
	s, err := OpenLegacy(t.TempDir(), Spec{Namespace: 1, Name: "image"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := s.db.NewBatch()
	marker := legacyKey(schema.EntityInvalid)
	if err := b.Set(marker[:], []byte{1, 10}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Set(legacyWitness[:], []byte{1}, nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 10; n++ {
		var v [14]byte
		v[0], v[1] = 1, byte(n)
		if n == 1 {
			binary.BigEndian.PutUint64(v[2:10], 1000)
		}
		binary.BigEndian.PutUint32(v[10:], crc32.ChecksumIEEE(v[:10]))
		key := legacyKey(schema.EntityKind(n))
		if err := b.Set(key[:], v[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	high, present, err := s.LegacyReservations()
	if err != nil || !present || high[1] != 1000 || high[schema.EntityUser] != 0 {
		t.Fatalf("v1 legacy: %v %v %+v", present, err, high)
	}
}
