package timeidx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineRebuildPublication(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	old, err := NewStore(dir, Spec{Namespace: 1, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	newIndex, err := NewStore(stage, Spec{Namespace: 1, Entries: []Entry{{Timestamp: 2, ImageID: 2}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if err := PublishRebuild(dir, stage, 1); !errors.Is(err, ErrStoreBusy) {
		t.Fatal(err)
	}
	old.Close()
	if err := PublishRebuild(dir, stage, 1); !errors.Is(err, ErrStoreBusy) {
		t.Fatal(err)
	}
	newIndex.Close()
	if err := PublishRebuild(dir, stage, 1); err != nil {
		t.Fatal(err)
	}
	i, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if got := i.Search(nil, 0, 3); len(got) != 1 || got[0] != 2 {
		t.Fatal(got)
	}
}

func TestBadRebuildLeavesOriginalIntact(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	i, err := NewStore(dir, Spec{Namespace: 1, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i.Close()
	if err = os.WriteFile(filepath.Join(stage, NamespaceFilename(1)), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PublishRebuild(dir, stage, 1); !errors.Is(err, ErrRebuildRequired) {
		t.Fatal(err)
	}
	i, err = OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if i.Len() != 1 {
		t.Fatal("destroyed original")
	}
}
