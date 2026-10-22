package schema

import (
	"encoding/hex"
	"testing"
)

func TestDefinitionsV3ProjectionVersionRoundTrip(t *testing.T) {
	defs := NewDefinitions()
	if err := defs.Register(NamespaceDef{
		Namespace:         101,
		Name:              "image-exif",
		Kind:              KindKV,
		Entity:            EntityImage,
		ProjectionVersion: 7,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := defs.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got := uint16(data[4]) | uint16(data[5])<<8; got != schemaVersionV3 {
		t.Fatalf("definitions version = %d, want %d", got, schemaVersionV3)
	}
	var restored Definitions
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	def, ok := restored.Lookup(101)
	if !ok || def.ProjectionVersion != 7 {
		t.Fatalf("projection version = %+v, %v", def, ok)
	}
}

func TestDefinitionsV3Golden(t *testing.T) {
	defs := NewDefinitions()
	if err := defs.Register(NamespaceDef{
		Namespace:         100,
		Name:              "image-kv",
		Kind:              KindKV,
		Entity:            EntityImage,
		ProjectionVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := defs.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const golden = "4350534e030001000000640006010300080000000000696d6167652d6b76b0f4364b"
	if got := hex.EncodeToString(data); got != golden {
		t.Fatalf("definitions v3:\n got %s\nwant %s", got, golden)
	}
}
