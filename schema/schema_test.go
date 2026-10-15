package schema

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func testOptions(backend Backend) Options {
	d := NewDefinitions()
	_ = d.RegisterAll(NamespaceDef{Namespace: NamespaceTag, Name: "Tag", Kind: KindKV, Entity: EntityImage}, NamespaceDef{Namespace: NamespacePerson, Name: "Person", Kind: KindKV, Entity: EntityPerson})
	return Options{Backend: backend, Definitions: d, Entities: []EntityConfig{{Entity: EntityImage, ReserveSize: 8}, {Entity: EntityPerson, ReserveSize: 4}}}
}

func singleOptions(backend Backend, ns Namespace, name string, block uint64) Options {
	d := NewDefinitions()
	_ = d.Register(NamespaceDef{Namespace: ns, Name: name, Kind: KindKV, Entity: EntityImage})
	return Options{Backend: backend, Definitions: d, Entities: []EntityConfig{{Entity: EntityImage, ReserveSize: block}}}
}

func forBackends(t *testing.T, run func(t *testing.T, backend Backend)) {
	for _, b := range []Backend{BackendReadWrite, BackendMmap} {
		name := "readwrite"
		if b == BackendMmap {
			name = "mmap"
		}
		t.Run(name, func(t *testing.T) { run(t, b) })
	}
}

func TestAllocationAndCleanReopen(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if other, err := Open(path, opts); !errors.Is(err, ErrLocked) {
			if other != nil {
				other.Close()
			}
			t.Fatalf("second owner: %v", err)
		}
		if id, err := s.NextID(EntityImage); id != 1 || err != nil {
			t.Fatalf("first tag: %d %v", id, err)
		}
		if id, err := s.NextID(EntityPerson); id != 1 || err != nil {
			t.Fatalf("first person: %d %v", id, err)
		}
		if start, err := s.NextIDs(EntityImage, 17); start != 2 || err != nil {
			t.Fatalf("cross blocks: %d %v", start, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityImage); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		opts.Entities[0].ReserveSize = 13
		if err := opts.Definitions.Register(NamespaceDef{Namespace: 256, Name: "Image", Kind: KindKV, Entity: EntityFace}); err != nil {
			t.Fatal(err)
		}
		opts.Entities = append(opts.Entities, EntityConfig{Entity: EntityFace, ReserveSize: 16})
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != 19 || err != nil {
			t.Fatalf("clean checkpoint: %d %v", id, err)
		}
		if id, err := s.NextID(EntityFace); id != 1 || err != nil {
			t.Fatalf("new namespace: %d %v", id, err)
		}
		if err := s.EnsureMinimum(EntityPerson, 50000); err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityPerson); id != 50001 || err != nil {
			t.Fatalf("minimum: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if id, err := s.NextID(EntityPerson); id != 50002 || err != nil {
			t.Fatalf("second clean: %d %v", id, err)
		}
	})
}

func TestPreparedDefinitionsRecoverAndActivate(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		initial := testOptions(backend)
		s, err := Open(path, initial)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityImage); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		target := testOptions(BackendReadWrite)
		if err := target.Definitions.Register(NamespaceDef{Namespace: 400, Name: "secondary", Kind: KindKV, Entity: EntityImage}); err != nil {
			t.Fatal(err)
		}
		target.Prepare = true
		s, err = Open(path, target)
		if err != nil {
			t.Fatal(err)
		}
		if s.Generation() != 1 || s.PreparedDefinitions() == nil || s.Definitions().Len() != initial.Definitions.Len() {
			t.Fatal("prepared schema replaced active definitions")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// Recovery needs no caller-provided target; it is already in schema.dat.
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		if s.PreparedDefinitions() == nil || s.PreparedDefinitions().Len() != target.Definitions.Len() {
			t.Fatal("lost durable prepared target")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Activate(); err != nil {
			t.Fatal(err)
		}
		if s.PreparedDefinitions() != nil || s.Generation() != 3 || s.Definitions().Len() != target.Definitions.Len() {
			t.Fatal("activation not durable")
		}
		if id, err := s.NextID(EntityImage); id != 2 || err != nil {
			t.Fatalf("allocator regressed: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.PreparedDefinitions() != nil || s.Generation() != 3 || s.Definitions().Len() != target.Definitions.Len() {
			t.Fatal("reopened wrong active generation")
		}
		if id, err := s.NextID(EntityImage); id != 3 || err != nil {
			t.Fatalf("reopen allocator: %d %v", id, err)
		}
	})
}

func TestOpenPreviousActiveSchemaFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	s, err := Open(path, testOptions(BackendReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	for slot := 0; slot < 2; slot++ {
		var raw [superBytes]byte
		if _, err := f.ReadAt(raw[:], int64(slot*superBytes)); err != nil {
			t.Fatal(err)
		}
		h, payload, ok := storage.DecodeSuperblock(raw[:], superMagic, SchemaVersion)
		if !ok {
			t.Fatal("invalid newly created superblock")
		}
		h.Version = previousVersion
		if err := storage.EncodeSuperblock(raw[:], h, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(raw[:], int64(slot*superBytes)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatalf("previous supported format: %v", err)
	}
	defer s.Close()
	if s.PreparedDefinitions() != nil || s.Generation() != 1 {
		t.Fatal("v2 must reopen ACTIVE")
	}
}

func TestPreparedPublicationCrashRecovery(t *testing.T) {
	for _, point := range []string{"after directory sync", "after superblock sync"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.dat")
			s, err := Open(path, testOptions(BackendReadWrite))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opts := testOptions(BackendReadWrite)
			if err := opts.Definitions.Register(NamespaceDef{Namespace: 400, Name: "extra", Kind: KindKV, Entity: EntityImage}); err != nil {
				t.Fatal(err)
			}
			opts.Prepare = true
			opts.fault = func(stage string) error {
				if stage == point {
					return ErrCorrupt
				}
				return nil
			}
			if opened, err := Open(path, opts); err == nil {
				_ = opened.Close()
				t.Fatal("injection did not fire")
			}
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if s.Definitions().Len() != 2 {
				t.Fatal("old active definitions lost")
			}
			if point == "after directory sync" && s.PreparedDefinitions() != nil {
				t.Fatal("unpublished target became prepared")
			}
			if point == "after superblock sync" && s.PreparedDefinitions() == nil {
				t.Fatal("durable target lost")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActivationPublicationCrashRecovery(t *testing.T) {
	for _, point := range []string{"before superblock", "after superblock sync"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.dat")
			s, err := Open(path, testOptions(BackendReadWrite))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opts := testOptions(BackendReadWrite)
			if err := opts.Definitions.Register(NamespaceDef{Namespace: 400, Name: "extra", Kind: KindKV, Entity: EntityImage}); err != nil {
				t.Fatal(err)
			}
			opts.Prepare = true
			s, err = Open(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path, Options{fault: func(stage string) error {
				if stage == point {
					return ErrCorrupt
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Activate(); err == nil {
				t.Fatal("injection did not fire")
			}
			_ = s.Close()
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if point == "before superblock" && s.PreparedDefinitions() == nil {
				t.Fatal("prepared target disappeared")
			}
			if point == "after superblock sync" && (s.PreparedDefinitions() != nil || s.Definitions().Len() != 3) {
				t.Fatal("durable activation disappeared")
			}
		})
	}
}

func TestLegacyRetirementMarkerIsDurable(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		s, err := Open(path, testOptions(BackendReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		if s.LegacyRetired() {
			t.Fatal("new file marked migrated before cleanup")
		}
		if err := s.MarkLegacyRetired(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		if !s.LegacyRetired() || s.Definitions().Len() != 2 {
			t.Fatal("lost retirement marker or definitions")
		}
		if id, err := s.NextID(EntityImage); id != 1 || err != nil {
			t.Fatalf("allocator: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConfigurationAndOverflow(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		for _, bad := range []Options{
			{Backend: 9, Definitions: opts.Definitions},
			{Backend: b},
			{Backend: b, Definitions: opts.Definitions, Entities: []EntityConfig{{Entity: EntityTag, ReserveSize: 8}}},
		} {
			if s, err := Open(path, bad); err == nil {
				s.Close()
				t.Fatal("accepted invalid config")
			}
		}
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(0); !errors.Is(err, ErrNamespace) {
			t.Fatal(err)
		}
		if _, err := s.NextIDs(EntityImage, 0); !errors.Is(err, ErrCount) {
			t.Fatal(err)
		}
		if err := s.EnsureMinimum(EntityImage, math.MaxUint64-1); err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != math.MaxUint64 || err != nil {
			t.Fatalf("last ID: %d %v", id, err)
		}
		if id, err := s.NextID(EntityImage); id != 0 || !errors.Is(err, ErrExhausted) {
			t.Fatalf("overflow: %d %v", id, err)
		}
		if id, err := s.NextIDs(EntityPerson, math.MaxUint64); id != 1 || err != nil {
			t.Fatalf("max range: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wrong := testOptions(b)
		wrong.Definitions = NewDefinitions()
		_ = wrong.Definitions.RegisterAll(NamespaceDef{Namespace: NamespaceTag, Name: "renamed", Kind: KindKV, Entity: EntityImage}, NamespaceDef{Namespace: NamespacePerson, Name: "Person", Kind: KindKV, Entity: EntityPerson})
		if _, err := Open(path, wrong); !errors.Is(err, ErrNamespace) {
			t.Fatalf("renamed: %v", err)
		}
		wrong = testOptions(b)
		wrong.Definitions = NewDefinitions()
		_ = wrong.Definitions.Register(NamespaceDef{Namespace: NamespaceTag, Name: "Tag", Kind: KindKV, Entity: EntityImage})
		wrong.Entities = wrong.Entities[:1]
		if _, err := Open(path, wrong); !errors.Is(err, ErrNamespace) {
			t.Fatalf("removed namespace: %v", err)
		}
	})
}

func TestSharedEntityNamespacesAndPersistedOwnership(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		defs := NewDefinitions()
		if err := defs.RegisterAll(
			NamespaceDef{Namespace: 101, Name: "image-exif", Kind: KindKV, Entity: EntityImage},
			NamespaceDef{Namespace: 102, Name: "image-ai", Kind: KindKV, Entity: EntityImage},
			NamespaceDef{Namespace: 103, Name: "person", Kind: KindKV, Entity: EntityPerson},
		); err != nil {
			t.Fatal(err)
		}
		opts := Options{Backend: backend, Definitions: defs, Entities: []EntityConfig{{Entity: EntityImage, ReserveSize: 8}}}
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, ns := range []Namespace{101, 102} {
			def, ok := s.Lookup(ns)
			if !ok || def.Entity != EntityImage {
				t.Fatalf("namespace %d ownership: %+v %v", ns, def, ok)
			}
		}
		if id, err := s.NextID(EntityImage); id != 1 || err != nil {
			t.Fatalf("image: %d %v", id, err)
		}
		if id, err := s.NextID(EntityPerson); id != 1 || err != nil {
			t.Fatalf("person: %d %v", id, err)
		}
		if id, err := s.NextID(EntityImage); id != 2 || err != nil {
			t.Fatalf("second image namespace: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != 3 || err != nil {
			t.Fatalf("reopen: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		other := NewDefinitions()
		_ = other.RegisterAll(
			NamespaceDef{Namespace: 101, Name: "image-exif", Kind: KindKV, Entity: EntityImage},
			NamespaceDef{Namespace: 102, Name: "image-ai", Kind: KindKV, Entity: EntityPerson},
			NamespaceDef{Namespace: 103, Name: "person", Kind: KindKV, Entity: EntityPerson},
		)
		if _, err := Open(path, Options{Backend: backend, Definitions: other}); !errors.Is(err, ErrNamespace) {
			t.Fatalf("reassigned owner: %v", err)
		}
	})
}

func TestOpenFromPersistedDefinitions(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		s, err := Open(path, testOptions(backend))
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); err != nil || id != 1 {
			t.Fatalf("initial ID: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		defs := s.Definitions()
		def, ok := defs.Lookup(NamespaceTag)
		if !ok || def.Entity != EntityImage {
			t.Fatalf("persisted owner: %+v %v", def, ok)
		}
		if id, err := s.NextID(EntityImage); err != nil || id != 2 {
			t.Fatalf("reopen ID: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := Open(filepath.Join(t.TempDir(), "missing"), Options{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing persisted schema created: %v", err)
	}
}

func TestAllocatorVersionOneRejectedWithoutModification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	opts := testOptions(BackendReadWrite)
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, superBytes} {
		before[offset+4] = 1
		before[offset+5] = 0
	}
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, opts); !errors.Is(err, ErrVersion) {
		t.Fatalf("v1 file must not be reinterpreted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("opening incompatible file modified it")
	}
}

func TestPersistedDefinitionsCorruptionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	opts := testOptions(BackendReadWrite)
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sb [superBytes]byte
	if _, err := f.ReadAt(sb[:], 0); err != nil {
		t.Fatal(err)
	}
	// The superblock payload begins after its 28-byte header. The definitions
	// offset is at payload byte 32 and is checked by the superblock checksum.
	off := binary.LittleEndian.Uint64(sb[28+32 : 28+40])
	var changed [1]byte
	if _, err := f.ReadAt(changed[:], int64(off)); err != nil {
		t.Fatal(err)
	}
	changed[0] ^= 0xff
	if _, err := f.WriteAt(changed[:], int64(off)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, opts); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt definitions: %v", err)
	}
}

func TestStandaloneKVAdditionActivatesImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	s, err := Open(path, testOptions(BackendReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.NextID(EntityImage); id != 1 || err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(BackendReadWrite)
	if err := opts.Definitions.Register(NamespaceDef{Namespace: 400, Name: "image-alt", Kind: KindKV, Entity: EntityImage}); err != nil {
		t.Fatal(err)
	}
	// KV-only additions own no dependent storage, so they activate at Open.
	s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.PreparedDefinitions() != nil {
		t.Fatal("KV-only addition left a prepared generation")
	}
	if _, ok := s.Lookup(400); !ok || s.Definitions().Len() != 3 {
		t.Fatal("KV addition missing from active schema")
	}
	if id, err := s.NextID(EntityImage); id != 2 || err != nil {
		t.Fatalf("existing sequence changed: %d %v", id, err)
	}
}

func TestStandaloneIndexAdditionRequiresActivate(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		s, err := Open(path, testOptions(BackendReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		opts := testOptions(BackendReadWrite)
		if err := opts.Definitions.Register(NamespaceDef{Namespace: 401, Name: "created", Kind: KindTimestamp, Entity: EntityImage}); err != nil {
			t.Fatal(err)
		}
		// A non-KV namespace needs an index engine, so Open must not activate it.
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if s.PreparedDefinitions() == nil {
			_ = s.Close()
			t.Fatal("index addition activated without external initialization")
		}
		if _, ok := s.Lookup(401); ok || s.Definitions().Len() != 2 {
			_ = s.Close()
			t.Fatal("prepared namespace became visible before activation")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// Reopening without definitions recovers the durable target.
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		if s.PreparedDefinitions() == nil {
			_ = s.Close()
			t.Fatal("lost prepared target across reopen")
		}
		if err := s.Activate(); err != nil {
			t.Fatal(err)
		}
		if s.PreparedDefinitions() != nil {
			t.Fatal("activation left prepared state")
		}
		if _, ok := s.Lookup(401); !ok || s.Definitions().Len() != 3 {
			t.Fatal("activated namespace missing")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.PreparedDefinitions() != nil || s.Definitions().Len() != 3 {
			t.Fatal("activated generation not durable")
		}
	})
}

func TestStandaloneAdditionPreservesExistingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	s, err := Open(path, testOptions(BackendReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := s.NextIDs(EntityImage, 5); id != 1 || err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		def  NamespaceDef
	}{
		{"renamed", NamespaceDef{Namespace: NamespaceTag, Name: "renamed", Kind: KindKV, Entity: EntityImage}},
		{"owner", NamespaceDef{Namespace: NamespaceTag, Name: "Tag", Kind: KindKV, Entity: EntityFace}},
		{"kind", NamespaceDef{Namespace: NamespaceTag, Name: "Tag", Kind: KindTimestamp, Entity: EntityImage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions(BackendReadWrite)
			opts.Definitions = NewDefinitions()
			_ = opts.Definitions.RegisterAll(tc.def, NamespaceDef{Namespace: NamespacePerson, Name: "Person", Kind: KindKV, Entity: EntityPerson})
			if opened, err := Open(path, opts); err == nil {
				_ = opened.Close()
				t.Fatal("accepted a modified existing definition")
			}
		})
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Definitions().Len() != 2 {
		t.Fatal("rejected change altered active schema")
	}
	if id, err := s.NextID(EntityImage); id != 6 || err != nil {
		t.Fatalf("allocation high-water regressed: %d %v", id, err)
	}
}
