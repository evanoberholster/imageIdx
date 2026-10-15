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
	for _, name := range []string{goldenClean, goldenUnclean, goldenPrepared, goldenActivated, goldenV2, goldenUnsupported} {
		data := buildFixture(t, name)
		if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: len=%d crc32c=%#08x", name, len(data), crc32.ChecksumIEEE(data))
	}
}

func TestGoldenFixtureChecksums(t *testing.T) {
	for _, name := range []string{goldenClean, goldenUnclean, goldenPrepared, goldenActivated, goldenV2, goldenUnsupported} {
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
	s, err := Open(copyFixture(t, goldenV2), Options{})
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
