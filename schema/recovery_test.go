package schema

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func TestCrashSubprocessSkipsReservation(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		if os.Getenv("SCHEMA_CRASH_CHILD") == "1" {
			path = os.Getenv("SCHEMA_CRASH_PATH")
			s, err := Open(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			if id, err := s.NextID(EntityImage); id != 4 || err != nil {
				t.Fatalf("clean resume: %d %v", id, err)
			}
			os.Exit(0) // no clean checkpoint
		}
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			if _, err := s.NextID(EntityImage); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashSubprocessSkipsReservation$")
		cmd.Env = append(os.Environ(), "SCHEMA_CRASH_CHILD=1", "SCHEMA_CRASH_PATH="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("crash subprocess: %v: %s", err, out)
		}
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != 9 || err != nil {
			t.Fatalf("unclean reopen: %d %v", id, err)
		}
		_ = s.Close()
	})
}

func TestDirectoryPublicationFaults(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		for _, step := range []string{"after directory sync", "before superblock", "after superblock sync"} {
			t.Run(step, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "schema.dat")
				old := testOptions(b)
				s, err := Open(path, old)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.NextID(EntityImage); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				newOpts := testOptions(b)
				_ = newOpts.Definitions.Register(NamespaceDef{Namespace: 88, Name: "new", Kind: KindKV, Entity: EntityFace})
				newOpts.Entities = append(newOpts.Entities, EntityConfig{Entity: EntityFace, ReserveSize: 17})
				newOpts.fault = func(stage string) error {
					if stage == step {
						return errors.New("injected")
					}
					return nil
				}
				if candidate, err := Open(path, newOpts); err == nil {
					candidate.Close()
					t.Fatal("injection ignored")
				}
				newOpts.fault = nil
				s, err = Open(path, newOpts)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if id, err := s.NextID(EntityImage); id < 2 || err != nil {
					t.Fatalf("old ID lost/reused: %d %v", id, err)
				}
				if id, err := s.NextID(EntityFace); id != 1 || err != nil {
					t.Fatalf("new namespace: %d %v", id, err)
				}
			})
		}
	})
}

func TestCorruptFileFailsClosed(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		for _, part := range []string{"magic", "version", "directory", "metadata", "truncate"} {
			t.Run(part, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "schema.dat")
				opts := testOptions(b)
				s, err := Open(path, opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.NextID(EntityImage); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				var pos int64
				switch part {
				case "magic", "version":
					pos = 0
				case "directory":
					pos = int64(metaStart + 2*metadataPairBytes + 2)
				case "metadata":
					pos = metaStart + metadataPairBytes // namespace 4, second slot
				case "truncate":
					if err := f.Truncate(300); err != nil {
						t.Fatal(err)
					}
				}
				if part != "truncate" {
					if _, err := f.WriteAt([]byte{0xff}, pos); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Sync(); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := Open(path, opts); !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrVersion) {
					t.Fatalf("accepted corrupt %s: %v", part, err)
				}
			})
		}
	})
}

func TestFormatRoundTripsAndUnsupportedVersion(t *testing.T) {
	for _, m := range []allocationRecord{{1, 0, 0, false}, {23, 1040, 712, true}} {
		var buf [metadataBytes]byte
		encodeRecord(buf[:], m)
		got, ok := decodeRecord(buf[:])
		if !ok || got != m {
			t.Fatalf("metadata: %+v, %v", got, ok)
		}
		buf[9] ^= 1
		if _, ok := decodeRecord(buf[:]); ok {
			t.Fatal("accepted invalid checksum")
		}
	}
	var sb [superBytes]byte
	p := [32]byte{}
	binary.LittleEndian.PutUint64(p[24:], 2000)
	if err := storage.EncodeSuperblock(sb[:], storage.SuperblockHeader{Magic: superMagic, Version: SchemaVersion + 1, Generation: 1}, p[:]); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := parseSuper(sb[:], 2000); !errors.Is(err, ErrVersion) {
		t.Fatal(err)
	}
}

func TestBackendsShareExactOnDiskFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	for i, backend := range []Backend{BackendReadWrite, BackendMmap, BackendReadWrite} {
		s, err := Open(path, testOptions(backend))
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != uint64(i+1) || err != nil {
			t.Fatalf("backend %d: %d %v", backend, id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailedActivationDoesNotReuseCheckpoint(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityImage); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		opts.fault = func(stage string) error {
			if stage == "after metadata sync" {
				return errors.New("uncertain activation")
			}
			return nil
		}
		if reopened, err := Open(path, opts); err == nil {
			reopened.Close()
			t.Fatal("accepted injected failure")
		}
		opts.fault = nil
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if id, err := s.NextID(EntityImage); id != 9 || err != nil {
			t.Fatalf("uncertain activation reused ID: %d %v", id, err)
		}
	})
}

func TestFailedCloseCannotReuseIssuedID(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		s, err := Open(path, testOptions(b))
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id != 1 || err != nil {
			t.Fatalf("first: %d %v", id, err)
		}
		s.fault = func(stage string) error {
			if stage == "after metadata sync" {
				return errors.New("uncertain checkpoint")
			}
			return nil
		}
		if err := s.Close(); err == nil {
			t.Fatal("accepted failed close")
		}
		s, err = Open(path, testOptions(b))
		if err != nil {
			return
		} // fail closed is safe
		defer s.Close()
		if id, err := s.NextID(EntityImage); id <= 1 || err != nil {
			t.Fatalf("reused issued ID: %d %v", id, err)
		}
	})
}

func TestCorruptNewestSuperblockDoesNotRollBackDirectory(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		_ = opts.Definitions.Register(NamespaceDef{Namespace: 99, Name: "later", Kind: KindKV, Entity: EntityFace})
		opts.Entities = append(opts.Entities, EntityConfig{Entity: EntityFace, ReserveSize: 1024})
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(EntityFace); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{0}, superBytes); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, opts); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("rolled back published directory: %v", err)
		}
	})
}

func TestDirectorySemanticValidation(t *testing.T) {
	entries := []directoryEntry{{id: 7, name: "name", reserve: 64, offset: metaStart}}
	data, err := directoryBytes(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDirectory(data, metaStart+metadataPairBytes); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]byte){
		func(d []byte) { binary.LittleEndian.PutUint16(d[2:4], 0) },
		func(d []byte) { binary.LittleEndian.PutUint64(d[14:22], 1) },
		func(d []byte) { d[22] = 0xff },
	} {
		corrupt := append([]byte(nil), data...)
		change(corrupt)
		binary.LittleEndian.PutUint32(corrupt[len(corrupt)-4:], storage.Checksum(corrupt[:len(corrupt)-4]))
		if _, err := parseDirectory(corrupt, metaStart+metadataPairBytes); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("accepted semantically invalid directory: %v", err)
		}
	}
}
