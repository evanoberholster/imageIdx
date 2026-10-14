package store

import (
	"bytes"
	"errors"
	"math"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestKeyEncoding(t *testing.T) {
	keys := []Key{
		{Namespace: 1, ID: 0, Flags: 0},
		{Namespace: 1, ID: 0, Flags: 1},
		{Namespace: 1, ID: 1, Flags: 0},
		{Namespace: 1, ID: math.MaxUint64, Flags: math.MaxUint16},
		{Namespace: 2, ID: 0, Flags: 0},
	}
	var previous [KeySize]byte
	for i, key := range keys {
		encoded := EncodeKey(key)
		decoded, err := DecodeKey(encoded[:])
		if err != nil || decoded != key {
			t.Fatalf("round trip %v: got %v, %v", key, decoded, err)
		}
		if i > 0 && bytes.Compare(previous[:], encoded[:]) >= 0 {
			t.Fatalf("keys are not ordered: %v >= %v", previous, encoded)
		}
		previous = encoded
	}
	if _, err := DecodeKey([]byte{1}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("short key: got %v", err)
	}
}

func TestStorePersistenceAndNamespaceScan(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{Namespace: 1, Name: "photos"}, {Namespace: schema.Namespace(math.MaxUint16), Name: "last"}}
	s, err := Open(dir, specs...)
	if err != nil {
		t.Fatal(err)
	}
	keys := []Key{
		{Namespace: 1, ID: 2, Flags: 1},
		{Namespace: 1, ID: 1, Flags: 2},
		{Namespace: 1, ID: 1, Flags: 1},
		{Namespace: schema.Namespace(math.MaxUint16), ID: 5, Flags: 0},
	}
	for _, key := range keys {
		if err := s.Set(key, []byte{byte(key.Flags)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, specs...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	value, err := s.Get(keys[0])
	if err != nil || !bytes.Equal(value, []byte{1}) {
		t.Fatalf("get after reopen: %v, %v", value, err)
	}
	value[0] = 9
	value, err = s.Get(keys[0])
	if err != nil || value[0] != 1 {
		t.Fatalf("get owns its value: %v, %v", value, err)
	}
	var got []Key
	if err := s.Scan(1, func(key Key, _ []byte) error {
		got = append(got, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []Key{keys[2], keys[1], keys[0]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	got = nil
	if err := s.Scan(schema.Namespace(math.MaxUint16), func(key Key, _ []byte) error {
		got = append(got, key)
		return nil
	}); err != nil || len(got) != 1 || got[0] != keys[3] {
		t.Fatalf("highest namespace scan: %v, %v", got, err)
	}
	if err := s.Delete(keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(keys[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
	if err := s.Set(Key{Namespace: 3}, nil); !errors.Is(err, ErrNamespace) {
		t.Fatalf("unregistered namespace: %v", err)
	}
}

func TestStoreRejectsDifferentSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Spec{Namespace: 1, Name: "photos"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Spec{Namespace: 1, Name: "videos"}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("mismatched schema: %v", err)
	}
	if _, err := Open(dir, Spec{Namespace: 0, Name: "invalid"}); !errors.Is(err, schema.ErrNamespaceInvalid) {
		t.Fatalf("invalid namespace: %v", err)
	}
}

func TestExplicitAppendOnlySchemaExtension(t *testing.T) {
	dir := t.TempDir()
	base := []Spec{{Namespace: 1, Name: "photos"}}
	s, err := Open(dir, base...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithExtension(dir, Spec{Namespace: 1, Name: "renamed"}, Spec{Namespace: 2, Name: "users"}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("changed old definition: %v", err)
	}
	if _, err := Open(dir, base[0], Spec{Namespace: 2, Name: "users"}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("implicit extension: %v", err)
	}
	s, err = OpenWithExtension(dir, base[0], Spec{Namespace: 2, Name: "users"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithExtension(dir, base...); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("removed definition: %v", err)
	}
}
