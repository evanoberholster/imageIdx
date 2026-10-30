package vectoridx

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

func TestFloat16Conversion(t *testing.T) {
	cases := []uint16{
		0x0000, 0x8000, 0x3c00, 0xbc00, 0x3800, 0x4000,
		0x0400, 0x03ff, 0x7bff, 0x7c00, 0xfc00, 0x7e00,
	}
	for _, h := range cases {
		f := Float16ToFloat32(h)
		got := Float32ToFloat16(f)
		if math.IsNaN(float64(f)) {
			if got&0x7c00 != 0x7c00 {
				t.Fatalf("NaN 0x%04x -> 0x%04x", h, got)
			}
			continue
		}
		if got != h {
			t.Fatalf("roundtrip 0x%04x -> %v -> 0x%04x", h, f, got)
		}
	}
	if got := Float32ToFloat16(65504); got != 0x7bff {
		t.Fatalf("max half = 0x%04x, want 0x7bff", got)
	}
	if got := Float32ToFloat16(65520); got != 0x7c00 {
		t.Fatalf("overflow = 0x%04x, want inf", got)
	}
	rng := rand.New(rand.NewSource(701))
	for range 1000 {
		f := float32(rng.NormFloat64() * 100)
		h := Float32ToFloat16(f)
		back := Float16ToFloat32(h)
		if math.IsInf(float64(back), 0) {
			continue
		}
		if rel := math.Abs(float64(back-f)) / math.Max(1, math.Abs(float64(f))); rel > 0.001 {
			t.Fatalf("half roundtrip %v -> 0x%04x -> %v rel=%v", f, h, back, rel)
		}
	}
}

func TestFloat16DistanceKernels(t *testing.T) {
	a := []uint16{0x3c00, 0x4000, 0x3800} // 1, 2, 0.5
	b := []uint16{0x3c00, 0x4000, 0x3800}
	if got := SquaredL2Float16(a, b); got != 0 {
		t.Fatalf("identical L2 = %v", got)
	}
	if got := DotFloat16(a, a); math.Abs(got-5.25) > 1e-6 {
		t.Fatalf("dot = %v, want 5.25", got)
	}
	if got := CosineFloat16(a, a); got != 0 {
		t.Fatalf("cosine self = %v", got)
	}
}

func TestFloat16Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "float16.idx")
	cfg := testConfigFloat512()
	cfg.ElementType = ElementFloat16
	idx, err := NewStore(path, cfg, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(704))
	vec := make([]uint16, Dim512)
	for i := range vec {
		vec[i] = Float32ToFloat16(rng.Float32()*4 - 2)
	}
	if err := idx.AddFloat16(1, vec); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Config().ElementType; got != ElementFloat16 {
		t.Fatalf("reopened element = %v", got)
	}
	results, err := reopened.SearchFloat16(vec, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 1 {
		t.Fatalf("reopened search = %+v", results)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestSIMDFloat16KernelsMatchScalar(t *testing.T) {
	if !simdFloat16Supported {
		t.Skip("no FP16 SIMD kernel on this architecture")
	}
	rng := rand.New(rand.NewSource(702))
	for _, dims := range []int{int(Dim512), int(Dim768), int(Dim1024)} {
		a := make([]uint16, dims)
		b := make([]uint16, dims)
		for i := range a {
			a[i] = Float32ToFloat16(rng.Float32()*4 - 2)
			b[i] = Float32ToFloat16(rng.Float32()*4 - 2)
		}
		rawA := encodeRawFloat16(nil, a)
		rawB := encodeRawFloat16(nil, b)
		wantL2 := SquaredL2Float16(a, b)
		gotL2 := simdL2Float16Bytes(rawA, rawB)
		if math.Abs(gotL2-wantL2) > 1e-3*math.Max(1, math.Abs(wantL2)) {
			t.Fatalf("SIMD FP16 L2 dim %d = %v, want %v", dims, gotL2, wantL2)
		}
		wantDot := DotFloat16(a, b)
		gotDot := simdDotFloat16Bytes(rawA, rawB)
		if math.Abs(gotDot-wantDot) > 1e-3*math.Max(1, math.Abs(wantDot)) {
			t.Fatalf("SIMD FP16 dot dim %d = %v, want %v", dims, gotDot, wantDot)
		}
	}
}

func TestFloat16VectorSearch(t *testing.T) {
	cfg := testConfigFloat512()
	cfg.ElementType = ElementFloat16
	idx, err := NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(703))
	vecs := make([][]uint16, 20)
	for i := range vecs {
		vecs[i] = make([]uint16, Dim512)
		for j := range vecs[i] {
			vecs[i][j] = Float32ToFloat16(rng.Float32()*4 - 2)
		}
		if err := idx.AddFloat16(NodeID(i+1), vecs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
	got, err := idx.SearchFloat16(vecs[0], nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ValueID != 1 {
		t.Fatalf("search = %+v", got)
	}
	exact, err := idx.ExactSearchFloat16(vecs[0], nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range exact {
		if exact[i].ValueID != got[i].ValueID {
			t.Fatalf("exact %d = %+v, ann = %+v", i, exact[i], got[i])
		}
	}
}
