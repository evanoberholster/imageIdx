package vectoridx

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelMetadata(t *testing.T) {
	const model = "siglip2-base-patch16-224"
	cfg := testConfigFloat512()
	cfg.Model = model
	idx, err := NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Model(); got != model {
		t.Fatalf("Model = %q, want %q", got, model)
	}
	if got := idx.Config().Model; got != model {
		t.Fatalf("Config.Model = %q, want %q", got, model)
	}
	if got := idx.Metadata().Model; got != model {
		t.Fatalf("Metadata.Model = %q, want %q", got, model)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModelPersistence(t *testing.T) {
	const model = "siglip2-base-patch16-224"
	path := filepath.Join(t.TempDir(), "model.idx")
	cfg := testConfigFloat512()
	cfg.Model = model
	idx, err := NewStore(path, cfg, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(1)), Dim512))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Model(); got != model {
		t.Fatalf("reopened Model = %q, want %q", got, model)
	}
	if got := reopened.Metadata().Model; got != model {
		t.Fatalf("reopened Metadata.Model = %q, want %q", got, model)
	}
}

func TestModelTooLong(t *testing.T) {
	cfg := testConfigFloat512()
	cfg.Model = strings.Repeat("x", MaxModelBytes+1)
	if _, err := NewMemory(cfg); err != ErrInvalidConfig {
		t.Fatalf("NewMemory = %v, want ErrInvalidConfig", err)
	}
}
