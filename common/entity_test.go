package common

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestEntityKindValidityAndNames(t *testing.T) {
	kinds := []EntityKind{
		EntityImage, EntityFace, EntityPerson, EntityObject, EntityAlbum,
		EntityEvent, EntityPlace, EntityAsset, EntityCollection, EntityStack,
	}
	if EntityKindCount != len(kinds) {
		t.Fatalf("EntityKindCount = %d, want %d", EntityKindCount, len(kinds))
	}
	seen := map[string]bool{}
	for n, kind := range kinds {
		if !kind.Valid() {
			t.Fatalf("kind %d invalid", kind)
		}
		if int(kind) != n+1 {
			t.Fatalf("kind %d not stable at ordinal %d", kind, n+1)
		}
		name := kind.String()
		if name == "invalid" || seen[name] {
			t.Fatalf("kind %d has unstable/duplicate name %q", kind, name)
		}
		seen[name] = true
	}
	if EntityInvalid.Valid() || EntityInvalid.String() != "invalid" {
		t.Fatal("EntityInvalid must be invalid")
	}
	if EntityKind(20).Valid() {
		t.Fatal("unknown entity kind accepted")
	}
}

func TestEntityAliasesAreValueID(t *testing.T) {
	// Compile-time proof that every entity identifier is the shared ValueID.
	var (
		_ ImageID      = ValueID(1)
		_ FaceID       = ValueID(1)
		_ PersonID     = ValueID(1)
		_ ObjectID     = ValueID(1)
		_ AlbumID      = ValueID(1)
		_ EventID      = ValueID(1)
		_ PlaceID      = ValueID(1)
		_ AssetID      = ValueID(1)
		_ CollectionID = ValueID(1)
		_ StackID      = ValueID(1)
	)
}

func TestSchemaEntityRoundTrip(t *testing.T) {
	schema := NewSchema()
	if err := schema.RegisterAll(
		NamespaceDef{Namespace: 1, Name: "time", Kind: KindTimestamp, Entity: EntityImage},
		NamespaceDef{Namespace: 2, Name: "faces", Kind: KindVector, Entity: EntityFace},
	); err != nil {
		t.Fatal(err)
	}
	data, err := schema.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var restored Schema
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if def, ok := restored.Lookup(1); !ok || def.Entity != EntityImage {
		t.Fatalf("entity round-trip = %+v, %v", def, ok)
	}
	if def, ok := restored.Lookup(2); !ok || def.Entity != EntityFace {
		t.Fatalf("entity round-trip = %+v, %v", def, ok)
	}
}

// TestSchemaLegacyVersionReadsEntityInvalid verifies a version-1 schema blob
// (identical layout, zero entity byte) still loads and yields EntityInvalid
// rather than silently reinterpreting the reserved byte.
func TestSchemaLegacyVersionReadsEntityInvalid(t *testing.T) {
	schema := NewSchema()
	if err := schema.Register(NamespaceDef{Namespace: 3, Name: "cap", Kind: KindTimestamp}); err != nil {
		t.Fatal(err)
	}
	data, err := schema.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(nil), data[:len(data)-4]...)
	binary.LittleEndian.PutUint16(body[4:6], schemaVersionLegacy)
	legacy := binary.LittleEndian.AppendUint32(body, crc32.ChecksumIEEE(body))

	var restored Schema
	if err := restored.UnmarshalBinary(legacy); err != nil {
		t.Fatal(err)
	}
	if def, ok := restored.Lookup(3); !ok || def.Entity != EntityInvalid {
		t.Fatalf("legacy entity = %+v, %v; want EntityInvalid", def, ok)
	}
}

func TestSchemaRejectsInvalidEntity(t *testing.T) {
	schema := NewSchema()
	if err := schema.Register(NamespaceDef{Namespace: 4, Name: "bad", Kind: KindTimestamp, Entity: EntityKind(200)}); !errors.Is(err, ErrNamespaceEntity) {
		t.Fatalf("err = %v, want ErrNamespaceEntity", err)
	}
}
