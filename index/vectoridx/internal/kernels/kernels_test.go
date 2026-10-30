package kernels

import (
	"math"
	"math/rand"
	"testing"
)

func TestFloat32KernelsStayAccurate(t *testing.T) {
	rng := rand.New(rand.NewSource(43))
	// Include lengths above and below the 4-way unrolled body so the tail
	// path is exercised too.
	for _, n := range []int{1, 3, 4, 5, 7, 512, 768, 769, 1024} {
		a := make([]float32, n)
		b := make([]float32, n)
		var wantL2, wantDot, wantNorm float64
		for i := range a {
			a[i] = rng.Float32()*4 - 2
			b[i] = rng.Float32()*4 - 2
			d := float64(a[i]) - float64(b[i])
			wantL2 += d * d
			wantDot += float64(a[i]) * float64(b[i])
			wantNorm += float64(a[i]) * float64(a[i])
		}
		wantNorm = math.Sqrt(wantNorm)
		if got := SquaredL2Float32(a, b); math.Abs(got-wantL2) > 1e-12*math.Max(1, math.Abs(wantL2)) {
			t.Fatalf("L2 n=%d = %v, want %v", n, got, wantL2)
		}
		if got := DotFloat32(a, b); math.Abs(got-wantDot) > 1e-12*math.Max(1, math.Abs(wantDot)) {
			t.Fatalf("dot n=%d = %v, want %v", n, got, wantDot)
		}
		if got := normFloat32(a); math.Abs(got-wantNorm) > 1e-12*math.Max(1, wantNorm) {
			t.Fatalf("norm n=%d = %v, want %v", n, got, wantNorm)
		}
	}
}

func TestSIMDFloat32KernelsMatchScalar(t *testing.T) {
	if !simdSupported {
		t.Skip("no FP32 SIMD kernel on this architecture")
	}
	rng := rand.New(rand.NewSource(44))
	for _, dims := range []int{512, 768, 1024} {
		a := make([]float32, dims)
		b := make([]float32, dims)
		for i := range a {
			a[i] = rng.Float32()*4 - 2
			b[i] = rng.Float32()*4 - 2
		}
		rawA := make([]byte, dims*4)
		rawB := make([]byte, dims*4)
		copy(BytesAsFloat32(rawA, dims), a)
		copy(BytesAsFloat32(rawB, dims), b)
		wantL2 := SquaredL2Float32(a, b)
		gotL2 := simdL2Float32Bytes(rawA, rawB)
		if math.Abs(gotL2-wantL2) > 1e-4*math.Max(1, math.Abs(wantL2)) {
			t.Fatalf("SIMD L2 dim %d = %v, want %v", dims, gotL2, wantL2)
		}
		wantDot := DotFloat32(a, b)
		gotDot := simdDotFloat32Bytes(rawA, rawB)
		if math.Abs(gotDot-wantDot) > 1e-4*math.Max(1, math.Abs(wantDot)) {
			t.Fatalf("SIMD dot dim %d = %v, want %v", dims, gotDot, wantDot)
		}
	}
}

func TestSIMDInt8KernelsMatchScalar(t *testing.T) {
	if !simdInt8Supported {
		t.Skip("no int8 SIMD kernel on this architecture")
	}
	rng := rand.New(rand.NewSource(45))
	for _, dims := range []int{512, 768, 1024} {
		a := make([]int8, dims)
		b := make([]int8, dims)
		au := make([]uint8, dims)
		bu := make([]uint8, dims)
		for i := range a {
			a[i] = int8(rng.Intn(256) - 128)
			b[i] = int8(rng.Intn(256) - 128)
			au[i] = uint8(rng.Intn(256))
			bu[i] = uint8(rng.Intn(256))
		}
		rawA := make([]byte, dims)
		rawB := make([]byte, dims)
		copy(BytesAsInt8(rawA, dims), a)
		copy(BytesAsInt8(rawB, dims), b)
		if got, want := simdDotInt8Bytes(rawA, rawB), DotInt8(a, b); got != want {
			t.Fatalf("SIMD int8 dot dim %d = %v, want %v", dims, got, want)
		}
		if got, want := simdL2Int8Bytes(rawA, rawB), SquaredL2Int8(a, b); got != want {
			t.Fatalf("SIMD int8 L2 dim %d = %v, want %v", dims, got, want)
		}
		rawAU := make([]byte, dims)
		rawBU := make([]byte, dims)
		copy(BytesAsUint8(rawAU, dims), au)
		copy(BytesAsUint8(rawBU, dims), bu)
		if got, want := simdDotUint8Bytes(rawAU, rawBU), DotUint8(au, bu); got != want {
			t.Fatalf("SIMD uint8 dot dim %d = %v, want %v", dims, got, want)
		}
		if got, want := simdL2Uint8Bytes(rawAU, rawBU), SquaredL2Uint8(au, bu); got != want {
			t.Fatalf("SIMD uint8 L2 dim %d = %v, want %v", dims, got, want)
		}
	}
}

func TestSIMDFloat16KernelsMatchScalar(t *testing.T) {
	if !simdFloat16Supported {
		t.Skip("no FP16 SIMD kernel on this architecture")
	}
	rng := rand.New(rand.NewSource(702))
	for _, dims := range []int{512, 768, 1024} {
		a := make([]uint16, dims)
		b := make([]uint16, dims)
		for i := range a {
			a[i] = Float32ToFloat16(rng.Float32()*4 - 2)
			b[i] = Float32ToFloat16(rng.Float32()*4 - 2)
		}
		rawA := make([]byte, dims*2)
		rawB := make([]byte, dims*2)
		copy(BytesAsUint16(rawA, dims), a)
		copy(BytesAsUint16(rawB, dims), b)
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

func TestDispatchMatrix(t *testing.T) {
	for _, e := range []Element{Int8, Uint8, Int16, Uint16, Float32, Float16} {
		if L2(e, 512) == nil || Dot(e, 512) == nil || NegDot(e, 512) == nil || Cosine(e, 512) == nil {
			t.Fatalf("missing kernel for element %d", e)
		}
	}
	if L2(Element(255), 512) != nil || Dot(Element(255), 512) != nil ||
		NegDot(Element(255), 512) != nil || Cosine(Element(255), 512) != nil {
		t.Fatal("expected nil kernel for unknown element")
	}
}
