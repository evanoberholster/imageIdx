package timeidx

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestProcessCrashRecovery(t *testing.T) {
	if dir := os.Getenv("TIMEIDX_CRASH_DIR"); dir != "" {
		i, err := NewStore(dir, Spec{Namespace: 1}, Config{MaxShardBytes: 512, MaxFileBytes: 4096}, DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		sourceApply(t, i, 1, Insert(1, 1))
		stage := os.Getenv("TIMEIDX_CRASH_STAGE")
		i.crash = func(s string) {
			if s == stage {
				os.Exit(83)
			}
		}
		b := NewBatch(1000)
		for n := uint64(2); n < 1002; n++ {
			b.Append(Insert(n, n))
		}
		_ = i.ApplyWithGeneration(b, 2)
		os.Exit(0)
	}
	for _, stage := range []string{"write", "data-sync", "commit", "root-sync", "publish", "reclaim", "grow", "mmap"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCrashRecovery$")
			cmd.Env = append(os.Environ(), "TIMEIDX_CRASH_DIR="+dir, "TIMEIDX_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("child didn't crash %s", out)
			}
			i, err := OpenNamespace(dir, 1, Config{MaxShardBytes: 512}, DurabilitySync)
			if err != nil {
				t.Fatalf("reopen %v: %s", err, out)
			}
			defer i.Close()
			if got := i.Search(nil, 1, 2); !slices.Equal(got, []uint64{1}) {
				t.Fatal("lost acknowledged write", got)
			}
			if stage == "publish" || stage == "reclaim" {
				if i.Len() != 1001 {
					t.Fatal("lost durable candidate")
				}
			}
			if stage == "write" || stage == "data-sync" || stage == "commit" || stage == "grow" || stage == "mmap" {
				if i.Len() != 1 || i.SourceGeneration() != 1 {
					t.Fatal("advanced before root publication", i.Len(), i.SourceGeneration())
				}
			}
			// root-sync terminates before fsync: either coherent root is legal.
			if i.Len() != 1 && i.Len() != 1001 {
				t.Fatal("partial batch", i.Len())
			}
			if (i.Len() == 1 && i.SourceGeneration() != 1) || (i.Len() == 1001 && i.SourceGeneration() != 2) {
				t.Fatal("partial watermark", i.Len(), i.SourceGeneration())
			}
			apply(t, i, Insert(100, 100))
		})
	}
}

func TestPinnedReaderGrowthAndClose(t *testing.T) {
	i := persistent(t, Config{MaxShardBytes: 512, MaxFileBytes: 4096})
	apply(t, i, Insert(1, 1))
	token := i.readers.Acquire()
	old := i.view.Load()
	done := make(chan error, 1)
	b := NewBatch(1000)
	for n := uint64(2); n < 1002; n++ {
		b.Append(Insert(n, n))
	}
	published := make(chan struct{})
	var once sync.Once
	i.crash = func(stage string) {
		if stage == "reclaim" {
			once.Do(func() { close(published) })
		}
	}
	go func() { done <- i.Apply(b) }()
	<-published
	if old.shards[0].times[0] != 1 || old.shards[0].ids[0] != 1 {
		t.Fatal("old mapping invalid")
	}
	select {
	case <-done:
		t.Fatal("reclaimed pinned reader")
	default:
	}
	i.readers.Release(token)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	i.crash = nil
	if err := VerifyNamespace(filepath.Dir(i.path), 1); err != nil {
		t.Fatal(err)
	}
	token = i.readers.Acquire()
	closed := make(chan error, 1)
	go func() { closed <- i.Close() }()
	// A new search never touches a mapping after closed is observed.
	i.readers.Release(token)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceCreationCrash(t *testing.T) {
	if dir := os.Getenv("TIMEIDX_CREATE_DIR"); dir != "" {
		stage := os.Getenv("TIMEIDX_CREATE_STAGE")
		_, err := createStore(dir, Spec{Namespace: 1, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Config{}, DurabilitySync, func(i *Index) {
			i.crash = func(s string) {
				if s == stage {
					os.Exit(84)
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	for _, stage := range []string{"allocate", "write", "directory", "data-sync", "commit", "root-sync", "publish", "create-name", "create-sync"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestNamespaceCreationCrash$")
			cmd.Env = append(os.Environ(), "TIMEIDX_CREATE_DIR="+dir, "TIMEIDX_CREATE_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("child didn't crash %s", out)
			}
			i, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
			if stage == "create-sync" {
				if err != nil {
					t.Fatal(err)
				}
				if i.Len() != 1 {
					t.Fatal(i.Len())
				}
				i.Close()
			} else {
				if err != ErrNamespaceNotFound {
					if i != nil {
						i.Close()
					}
					t.Fatal("incomplete namespace visible", err)
				}
				i, err = NewStore(dir, Spec{Namespace: 1}, Config{}, DurabilitySync)
				if err != nil {
					t.Fatal("retry creation", err)
				}
				i.Close()
			}
		})
	}
}
