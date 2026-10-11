package common

import (
	"errors"
	"slices"
	"testing"
)

func TestSchemaRegistrationAndUniqueness(t *testing.T) {
	schema := NewSchema()
	defs := []NamespaceDef{
		{Namespace: NamespaceLocation, Name: "location", Kind: KindBitmapGeo, Config: []byte{1, 2, 3}},
		{Namespace: NamespaceTag, Name: "tag", Kind: KindBitmapEnum},
		{Namespace: 90, Name: "timeline", Kind: KindTimestamp},
		{Namespace: 100, Name: "vec-image", Kind: KindVector},
	}
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatal(err)
	}
	if schema.Len() != len(defs) {
		t.Fatalf("Len = %d", schema.Len())
	}
	if def, ok := schema.Lookup(NamespaceLocation); !ok || def.Name != "location" || !slices.Equal(def.Config, []byte{1, 2, 3}) {
		t.Fatalf("lookup = %+v, %v", def, ok)
	}
	if def, ok := schema.LookupName("vec-image"); !ok || def.Namespace != 100 {
		t.Fatalf("name lookup = %+v, %v", def, ok)
	}
	if _, ok := schema.Lookup(0); ok {
		t.Fatal("zero namespace resolved")
	}
	if _, ok := schema.LookupName("missing"); ok {
		t.Fatal("missing name resolved")
	}
	if got := len(schema.DefsOfKind(KindBitmapEnum)); got != 1 {
		t.Fatalf("enum defs = %d", got)
	}
	if got := len(schema.DefsOfKind(KindBitmapGeo)); got != 1 {
		t.Fatalf("geo defs = %d", got)
	}

	if err := schema.Register(NamespaceDef{Namespace: NamespaceLocation, Name: "other", Kind: KindBitmapEnum}); !errors.Is(err, ErrNamespaceDuplicate) {
		t.Fatalf("duplicate id err = %v", err)
	}
	if err := schema.Register(NamespaceDef{Namespace: 500, Name: "location", Kind: KindBitmapEnum}); !errors.Is(err, ErrNamespaceName) {
		t.Fatalf("duplicate name err = %v", err)
	}
	if err := schema.Register(NamespaceDef{Namespace: 0, Name: "zero", Kind: KindBitmapEnum}); !errors.Is(err, ErrNamespaceInvalid) {
		t.Fatalf("zero err = %v", err)
	}
	if err := schema.Register(NamespaceDef{Namespace: 501, Name: "", Kind: KindBitmapEnum}); !errors.Is(err, ErrNamespaceName) {
		t.Fatalf("empty name err = %v", err)
	}
	if err := schema.Register(NamespaceDef{Namespace: 502, Name: "bad", Kind: 99}); !errors.Is(err, ErrNamespaceKind) {
		t.Fatalf("bad kind err = %v", err)
	}
}

func TestSchemaRoundTrip(t *testing.T) {
	schema := NewSchema()
	if err := schema.RegisterAll(
		NamespaceDef{Namespace: NamespaceAlbum, Name: "album", Kind: KindBitmapEnum, Config: []byte("codec")},
		NamespaceDef{Namespace: 700, Name: "sha256", Kind: KindHashExact, Config: []byte{9}},
		NamespaceDef{Namespace: 701, Name: "pdq", Kind: KindHashPerceptual},
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
	if restored.Len() != schema.Len() {
		t.Fatalf("restored Len = %d", restored.Len())
	}
	got := restored.Defs()
	want := schema.Defs()
	for i := range want {
		if got[i].Namespace != want[i].Namespace || got[i].Name != want[i].Name || got[i].Kind != want[i].Kind || !slices.Equal(got[i].Config, want[i].Config) {
			t.Fatalf("def %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Deterministic: marshaling twice must be byte-identical.
	again, err := schema.MarshalBinary()
	if err != nil || !slices.Equal(data, again) {
		t.Fatal("marshal is not deterministic")
	}
}

func TestSchemaRejectsCorruption(t *testing.T) {
	schema := NewSchema()
	if err := schema.Register(NamespaceDef{Namespace: 1, Name: "a", Kind: KindBitmapEnum}); err != nil {
		t.Fatal(err)
	}
	data, err := schema.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"short":     data[:5],
		"truncated": data[:len(data)-2],
		"crc":       slices.Clone(data),
		"magic":     slices.Clone(data),
		"version":   slices.Clone(data),
	}
	cases["crc"][6] ^= 0xFF
	cases["magic"][0] ^= 0xFF
	cases["version"][4] = 9
	var target Schema
	for name, corrupted := range cases {
		if err := target.UnmarshalBinary(corrupted); !errors.Is(err, ErrCorruptSchema) {
			t.Fatalf("%s: err = %v, want ErrCorruptSchema", name, err)
		}
	}
	// A rejected unmarshal must not replace the receiver.
	if err := target.Register(NamespaceDef{Namespace: 2, Name: "keep", Kind: KindBitmapEnum}); err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(cases["crc"]); err == nil {
		t.Fatal("accepted corrupt schema")
	}
	if _, ok := target.LookupName("keep"); !ok {
		t.Fatal("failed unmarshal replaced the receiver")
	}
}

func TestSchemaDefensiveCopies(t *testing.T) {
	schema := NewSchema()
	config := []byte{1, 2, 3}
	if err := schema.Register(NamespaceDef{Namespace: 5, Name: "copy", Kind: KindVector, Config: config}); err != nil {
		t.Fatal(err)
	}
	config[0] = 9
	def, _ := schema.Lookup(5)
	if def.Config[0] != 1 {
		t.Fatal("Register retained caller config bytes")
	}
}

func TestNamespaceAndKindValidity(t *testing.T) {
	if InvalidNamespace.Valid() {
		t.Fatal("zero namespace is valid")
	}
	if !NamespaceTag.Valid() {
		t.Fatal("tag namespace invalid")
	}
	for _, kind := range []NamespaceKind{
		KindBitmapEnum, KindBitmapBool, KindBitmapString,
		KindBitmapContinuous, KindBitmapGeo, KindBitmapBSI, KindTimestamp, KindHashExact,
		KindHashPerceptual, KindVector, KindKV, KindGeo,
	} {
		if !kind.Valid() || kind.String() == "unknown" {
			t.Fatalf("kind %d valid=%v name=%q", kind, kind.Valid(), kind.String())
		}
	}
	if !KindBitmapEnum.IsBitmap() || !KindBitmapString.IsBitmap() || !KindBitmapBSI.IsBitmap() || KindVector.IsBitmap() {
		t.Fatal("bitmap kind classification is incorrect")
	}
	if NamespaceKind(0).Valid() || NamespaceKind(1).Valid() || NamespaceKind(0x10).Valid() ||
		NamespaceKind(0x17).Valid() || NamespaceKind(0x18).Valid() || NamespaceKind(0x19).Valid() || NamespaceKind(99).Valid() {
		t.Fatal("invalid kind accepted")
	}
}
