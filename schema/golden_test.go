package schema

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// This file defines the persistent-format contract for schema.dat. The
// committed testdata fixtures are byte-stable artifacts of a released format;
// they must keep opening with the same definitions and allocator state. Run
//
//	IMAGEIDX_UPDATE_GOLDEN=1 go test ./schema -run TestUpdateGoldenFixtures
//
// only when deliberately changing the on-disk format, and review the diff.

const (
	goldenClean       = "v3-clean.dat"
	goldenUnclean     = "v3-unclean.dat"
	goldenPrepared    = "v3-prepared.dat"
	goldenActivated   = "v3-active-after-prepare.dat"
	goldenV2          = "v2-active.dat"
	goldenUpgraded    = "v3-upgraded-from-v2.dat"
	goldenUnsupported = "v3-unsupported-version.dat"
)

// goldenChecksums pins the committed fixture bytes. A mismatch means the
// format writer changed, which requires an explicit format decision.
var goldenChecksums = map[string]uint32{
	goldenClean:       0x77ec7b7a,
	goldenUnclean:     0xbb26b838,
	goldenPrepared:    0x2562876c,
	goldenActivated:   0x0ac8147d,
	goldenV2:          0xdb7fdbd3,
	goldenUpgraded:    0x45fbfe56,
	goldenUnsupported: 0x2044f1ae,
}

func goldenOptions(backend Backend) Options {
	defs := NewDefinitions()
	_ = defs.RegisterAll(
		NamespaceDef{Namespace: 100, Name: "image-kv", Kind: KindKV, Entity: EntityImage},
		NamespaceDef{Namespace: 200, Name: "person-kv", Kind: KindKV, Entity: EntityPerson},
	)
	return Options{
		Backend:     backend,
		Definitions: defs,
		Entities:    []EntityConfig{{Entity: EntityImage, ReserveSize: 8}, {Entity: EntityPerson, ReserveSize: 4}},
	}
}

// buildFixture produces one canonical fixture byte image. It centralizes the
// otherwise error-prone allocator/activation sequence.
func buildFixture(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.dat")
	read := func() []byte {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	switch name {
	case goldenClean:
		s, err := Open(path, goldenOptions(BackendReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextIDs(EntityImage, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityPerson); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return read()
	case goldenUnclean:
		s, err := Open(path, goldenOptions(BackendReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextIDs(EntityImage, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityPerson); err != nil {
			t.Fatal(err)
		}
		// Capture the durable reservation slots without a clean-shutdown
		// checkpoint, then release the lock.
		data := read()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return data
	case goldenPrepared, goldenActivated:
		s, err := Open(path, goldenOptions(BackendReadWrite))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextIDs(EntityImage, 5); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		opts := goldenOptions(BackendReadWrite)
		if err := opts.Definitions.Register(NamespaceDef{Namespace: 401, Name: "created", Kind: KindTimestamp, Entity: EntityImage}); err != nil {
			t.Fatal(err)
		}
		opts.Prepare = true
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if name == goldenActivated {
			if err := s.Activate(); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return read()
	case goldenV2:
		clean := buildFixture(t, goldenClean)
		return rewriteVersion(t, clean, previousVersion)
	case goldenUpgraded:
		// A version-2 file upgraded in place to version 3 by the retirement
		// marker, with definitions and allocator state preserved.
		v2 := buildFixture(t, goldenV2)
		if err := os.WriteFile(path, v2, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if s.LegacyRetired() {
			t.Fatal("v2 fixture already retired")
		}
		if err := s.MarkLegacyRetired(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return read()
	case goldenUnsupported:
		clean := buildFixture(t, goldenClean)
		return rewriteVersion(t, clean, 99)
	default:
		t.Fatalf("unknown fixture %q", name)
		return nil
	}
}

// rewriteVersion re-encodes both superblocks at a different version while
// keeping the payload, producing compatibility and rejection fixtures.
func rewriteVersion(t testing.TB, data []byte, version uint16) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	for slot := 0; slot < 2; slot++ {
		off := slot * superBytes
		original := binary.LittleEndian.Uint16(out[off+4 : off+6])
		h, payload, ok := storage.DecodeSuperblock(out[off:off+superBytes], superMagic, original)
		if !ok {
			t.Fatal("invalid fixture superblock")
		}
		h.Version = version
		if err := storage.EncodeSuperblock(out[off:off+superBytes], h, payload); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func copyFixture(t testing.TB, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	path := filepath.Join(t.TempDir(), "schema.dat")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpdateGoldenFixtures(t *testing.T) {
	if os.Getenv("IMAGEIDX_UPDATE_GOLDEN") != "1" {
		t.Skip("set IMAGEIDX_UPDATE_GOLDEN=1 to regenerate fixtures")
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{goldenClean, goldenUnclean, goldenPrepared, goldenActivated, goldenV2, goldenUpgraded, goldenUnsupported} {
		data := buildFixture(t, name)
		if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: len=%d crc32c=%#08x", name, len(data), crc32.ChecksumIEEE(data))
	}
}

func TestGoldenFixtureChecksums(t *testing.T) {
	for _, name := range []string{goldenClean, goldenUnclean, goldenPrepared, goldenActivated, goldenV2, goldenUpgraded, goldenUnsupported} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		if want := goldenChecksums[name]; crc32.ChecksumIEEE(data) != want {
			t.Fatalf("%s changed: crc=%#08x want=%#08x; regenerate deliberately and update goldenChecksums", name, crc32.ChecksumIEEE(data), want)
		}
	}
}

func TestGoldenCleanAndUncleanRecovery(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		clean, err := Open(copyFixture(t, goldenClean), Options{Backend: backend})
		if err != nil {
			t.Fatalf("clean fixture: %v", err)
		}
		if clean.PreparedDefinitions() != nil || clean.Definitions().Len() != 2 || clean.Generation() != 1 {
			t.Fatal("clean fixture state")
		}
		if id, err := clean.NextID(EntityImage); id != 6 || err != nil {
			t.Fatalf("clean image resume: %d %v", id, err)
		}
		if id, err := clean.NextID(EntityPerson); id != 2 || err != nil {
			t.Fatalf("clean person resume: %d %v", id, err)
		}
		if err := clean.Close(); err != nil {
			t.Fatal(err)
		}
		unclean, err := Open(copyFixture(t, goldenUnclean), Options{Backend: backend})
		if err != nil {
			t.Fatalf("unclean fixture: %v", err)
		}
		defer unclean.Close()
		// Unclean recovery resumes at the durable reservation boundary: the
		// reserved block is 8 for image and 4 for person.
		if id, err := unclean.NextID(EntityImage); id != 9 || err != nil {
			t.Fatalf("unclean image resume: %d %v", id, err)
		}
		if id, err := unclean.NextID(EntityPerson); id != 5 || err != nil {
			t.Fatalf("unclean person resume: %d %v", id, err)
		}
	})
}

func TestGoldenPreparedAndActiveStateDecoding(t *testing.T) {
	prepared, err := Open(copyFixture(t, goldenPrepared), Options{})
	if err != nil {
		t.Fatalf("prepared fixture: %v", err)
	}
	if prepared.PreparedDefinitions() == nil || prepared.PreparedDefinitions().Len() != 3 {
		t.Fatal("prepared target not decoded")
	}
	if prepared.Definitions().Len() != 2 || prepared.Generation() != 1 {
		t.Fatal("prepared fixture exposed the target as active")
	}
	if _, ok := prepared.Lookup(401); ok {
		t.Fatal("prepared namespace visible before activation")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	activated, err := Open(copyFixture(t, goldenActivated), Options{})
	if err != nil {
		t.Fatalf("activated fixture: %v", err)
	}
	defer activated.Close()
	if activated.PreparedDefinitions() != nil || activated.Definitions().Len() != 3 || activated.Generation() != 3 {
		t.Fatal("activated fixture did not decode as active")
	}
	if def, ok := activated.Lookup(401); !ok || def.Kind != KindTimestamp || def.Entity != EntityImage {
		t.Fatalf("activated namespace: %+v %v", def, ok)
	}
}

func TestGoldenPreviousVersionIsActive(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		s, err := Open(copyFixture(t, goldenV2), Options{Backend: backend})
		if err != nil {
			t.Fatalf("v2 fixture: %v", err)
		}
		defer s.Close()
		if s.PreparedDefinitions() != nil || s.Definitions().Len() != 2 || s.LegacyRetired() {
			t.Fatal("v2 fixture state")
		}
		if id, err := s.NextID(EntityImage); id != 6 || err != nil {
			t.Fatalf("v2 resume: %d %v", id, err)
		}
	})
}

// activeContainerVersion returns the version of the highest-generation
// superblock. During a lazy upgrade one slot may briefly remain on the older
// version, so the active one is the meaningful answer.
func activeContainerVersion(t testing.TB, path string) uint16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var best uint16
	var bestGen uint64
	for slot := 0; slot < 2; slot++ {
		off := slot * superBytes
		version := binary.LittleEndian.Uint16(data[off+4 : off+6])
		h, _, ok := storage.DecodeSuperblock(data[off:off+superBytes], superMagic, version)
		if !ok {
			continue
		}
		if h.Generation >= bestGen {
			bestGen, best = h.Generation, version
		}
	}
	if best == 0 {
		t.Fatal("no valid superblock in fixture")
	}
	return best
}

// seedDefinitions returns the persisted definitions of a fixture copy plus the
// additive namespaces a caller wants to append, mirroring the real flow where a
// caller extends the active model.
func seedDefinitions(t testing.TB, path string, add ...NamespaceDef) *Definitions {
	t.Helper()
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defs := s.Definitions()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := defs.RegisterAll(add...); err != nil {
		t.Fatal(err)
	}
	return defs
}

// TestGoldenV2UpgradeOnRetirement upgrades a version-2 file to version 3 by
// publishing the retirement marker, then confirms identity and allocator state
// survive the format bump on both backends.
func TestGoldenV2UpgradeOnRetirement(t *testing.T) {
	forBackends(t, func(t *testing.T, backend Backend) {
		path := copyFixture(t, goldenV2)
		if got := activeContainerVersion(t, path); got != previousVersion {
			t.Fatalf("fixture version = %d, want %d", got, previousVersion)
		}
		s, err := Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		if s.Generation() != 1 || s.LegacyRetired() {
			t.Fatal("v2 state before upgrade")
		}
		if err := s.MarkLegacyRetired(); err != nil {
			t.Fatal(err)
		}
		if !s.LegacyRetired() || s.Generation() != 2 {
			t.Fatal("retirement marker did not advance the generation")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if got := activeContainerVersion(t, path); got != SchemaVersion {
			t.Fatalf("upgraded version = %d, want %d", got, SchemaVersion)
		}
		s, err = Open(path, Options{Backend: backend})
		if err != nil {
			t.Fatalf("reopen upgraded: %v", err)
		}
		defer s.Close()
		if !s.LegacyRetired() || s.PreparedDefinitions() != nil || s.Definitions().Len() != 2 {
			t.Fatal("upgraded state")
		}
		if id, err := s.NextID(EntityImage); id != 6 || err != nil {
			t.Fatalf("upgraded resume: %d %v", id, err)
		}
	})
}

// TestGoldenV2UpgradeOnAdditiveChange exercises the lazy upgrade: a version-2
// file that receives a new directory (a KV namespace) becomes version 3 while
// preserving its existing identities and high-water marks.
func TestGoldenV2UpgradeOnAdditiveChange(t *testing.T) {
	path := copyFixture(t, goldenV2)
	opts := Options{Definitions: seedDefinitions(t, path, NamespaceDef{Namespace: 300, Name: "added-kv", Kind: KindKV, Entity: EntityImage})}
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("additive upgrade: %v", err)
	}
	if s.PreparedDefinitions() != nil {
		t.Fatal("KV-only addition must activate immediately")
	}
	if _, ok := s.Lookup(300); !ok || s.Definitions().Len() != 3 {
		t.Fatal("added namespace missing after upgrade")
	}
	if id, err := s.NextID(EntityImage); id != 6 || err != nil {
		t.Fatalf("upgrade rewound image: %d %v", id, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := activeContainerVersion(t, path); got != SchemaVersion {
		t.Fatalf("additive upgrade version = %d, want %d", got, SchemaVersion)
	}
}

func TestGoldenUpgradedFixture(t *testing.T) {
	s, err := Open(copyFixture(t, goldenUpgraded), Options{})
	if err != nil {
		t.Fatalf("upgraded fixture: %v", err)
	}
	defer s.Close()
	if !s.LegacyRetired() || s.PreparedDefinitions() != nil || s.Definitions().Len() != 2 {
		t.Fatal("upgraded fixture state")
	}
	if id, err := s.NextID(EntityImage); id != 6 || err != nil {
		t.Fatalf("upgraded fixture resume: %d %v", id, err)
	}
}

// TestGoldenV2UpgradeCrashRecovery injects failures during the superblock
// publication that performs the v2 -> v3 upgrade and requires the file to
// remain a readable version (old active or new active) with no ID reuse.
func TestGoldenV2UpgradeCrashRecovery(t *testing.T) {
	for _, point := range []string{"before superblock", "after superblock sync"} {
		t.Run(point, func(t *testing.T) {
			path := copyFixture(t, goldenV2)
			opts := Options{Definitions: seedDefinitions(t, path, NamespaceDef{Namespace: 300, Name: "added-kv", Kind: KindKV, Entity: EntityImage}), fault: func(stage string) error {
				if stage == point {
					return ErrCorrupt
				}
				return nil
			}}
			if s, err := Open(path, opts); err == nil {
				_ = s.Close()
				t.Fatal("injection did not fire")
			}
			s, err := Open(path, Options{})
			if err != nil {
				t.Fatalf("reopen after injected upgrade crash: %v", err)
			}
			defer s.Close()
			if id, err := s.NextID(EntityImage); id <= 5 || err != nil {
				t.Fatalf("upgrade crash reused an ID: %d %v", id, err)
			}
			switch activeContainerVersion(t, path) {
			case previousVersion:
				if s.Definitions().Len() != 2 {
					t.Fatal("v2 retained target definitions")
				}
			case SchemaVersion:
				if _, ok := s.Lookup(300); !ok {
					t.Fatal("v3 active without the added namespace")
				}
			default:
				t.Fatal("unreadable container version after crash")
			}
		})
	}
}

func TestGoldenUnsupportedVersionFailsClosed(t *testing.T) {
	for _, name := range []string{goldenUnsupported} {
		if _, err := Open(copyFixture(t, name), Options{}); !errors.Is(err, ErrVersion) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestGoldenCorruptMetadataFailsClosed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", goldenClean))
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{superBytes + 40, 2*superBytes + 8, len(data) - 1} {
		corrupt := append([]byte(nil), data...)
		corrupt[offset] ^= 0xff
		path := filepath.Join(t.TempDir(), "schema.dat")
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path, Options{}); err == nil {
			_ = s.Close()
			t.Fatalf("offset %d: corrupt metadata accepted", offset)
		} else if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrNamespace) && !errors.Is(err, ErrVersion) {
			t.Fatalf("offset %d: untyped error %v", offset, err)
		}
	}
}
