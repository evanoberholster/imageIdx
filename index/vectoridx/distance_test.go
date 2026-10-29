package vectoridx

import (
	"math"
	"math/rand"
	"testing"
)

func TestTypedKernelsHandComputed(t *testing.T) {
	if got := SquaredL2Int8([]int8{1, 2, 3}, []int8{1, 2, 3}); got != 0 {
		t.Fatalf("int8 identical = %v, want 0", got)
	}
	if got := SquaredL2Int8([]int8{0, 0}, []int8{3, 4}); got != 25 {
		t.Fatalf("int8 (3,4) = %v, want 25", got)
	}
	if got := SquaredL2Uint8([]uint8{0, 255}, []uint8{255, 0}); got != 2*65025 {
		t.Fatalf("uint8 extremes = %v, want %v", got, 2*65025)
	}
	if got := SquaredL2Int16([]int16{-32768}, []int16{32767}); got != 4294836225 {
		t.Fatalf("int16 extremes = %v, want 4294836225", got)
	}
	if got := SquaredL2Uint16([]uint16{0}, []uint16{65535}); got != 4294836225 {
		t.Fatalf("uint16 extremes = %v, want 4294836225", got)
	}
	if got := SquaredL2Float32([]float32{1.5, -2.5}, []float32{1.5, -2.5}); got != 0 {
		t.Fatalf("float32 identical = %v, want 0", got)
	}
	if got := SquaredL2Float32([]float32{0}, []float32{0.5}); math.Abs(got-0.25) > 1e-12 {
		t.Fatalf("float32 half = %v, want 0.25", got)
	}
}

func TestRawKernelsMatchTyped(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, dims := range []int{int(Dim512), int(Dim768), int(Dim1024)} {
		// int8
		a8 := make([]int8, dims)
		b8 := make([]int8, dims)
		for i := range a8 {
			a8[i] = int8(rng.Intn(256) - 128)
			b8[i] = int8(rng.Intn(256) - 128)
		}
		rawA := make([]byte, dims)
		rawB := make([]byte, dims)
		copy(bytesAsInt8(rawA, dims), a8)
		copy(bytesAsInt8(rawB, dims), b8)
		if want, got := SquaredL2Int8(a8, b8), squaredL2Int8Bytes(rawA, rawB, dims); want != got {
			t.Fatalf("int8 dim %d: typed %v != raw %v", dims, want, got)
		}
		// uint8
		au := make([]uint8, dims)
		bu := make([]uint8, dims)
		for i := range au {
			au[i] = uint8(rng.Intn(256))
			bu[i] = uint8(rng.Intn(256))
		}
		if want, got := SquaredL2Uint8(au, bu), squaredL2Uint8Bytes(au, bu, dims); want != got {
			t.Fatalf("uint8 dim %d: typed %v != raw %v", dims, want, got)
		}
		// int16 incl. extremes
		ai := make([]int16, dims)
		bi := make([]int16, dims)
		for i := range ai {
			ai[i] = int16(rng.Intn(65536) - 32768)
			bi[i] = int16(rng.Intn(65536) - 32768)
		}
		ai[0], bi[0] = -32768, 32767
		rawAI := encodeRawInt16(nil, ai)
		rawBI := encodeRawInt16(nil, bi)
		if want, got := SquaredL2Int16(ai, bi), squaredL2Int16Bytes(rawAI, rawBI, dims); want != got {
			t.Fatalf("int16 dim %d: typed %v != raw %v", dims, want, got)
		}
		// uint16 incl. extremes
		ay := make([]uint16, dims)
		by := make([]uint16, dims)
		for i := range ay {
			ay[i] = uint16(rng.Intn(65536))
			by[i] = uint16(rng.Intn(65536))
		}
		ay[0], by[0] = 0, 65535
		rawAY := encodeRawUint16(nil, ay)
		rawBY := encodeRawUint16(nil, by)
		if want, got := SquaredL2Uint16(ay, by), squaredL2Uint16Bytes(rawAY, rawBY, dims); want != got {
			t.Fatalf("uint16 dim %d: typed %v != raw %v", dims, want, got)
		}
		// float32
		af := make([]float32, dims)
		bf := make([]float32, dims)
		for i := range af {
			af[i] = rng.Float32()*4 - 2
			bf[i] = rng.Float32()*4 - 2
		}
		rawAF := encodeRawFloat32(nil, af)
		rawBF := encodeRawFloat32(nil, bf)
		if want, got := SquaredL2Float32(af, bf), squaredL2Float32Bytes(rawAF, rawBF, dims); want != got {
			t.Fatalf("float32 dim %d: typed %v != raw %v", dims, want, got)
		}
	}
}

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
		t.Skip("no SIMD kernel on this architecture")
	}
	rng := rand.New(rand.NewSource(44))
	for _, dims := range []Dimensions{Dim512, Dim768, Dim1024} {
		a := make([]float32, dims)
		b := make([]float32, dims)
		for i := range a {
			a[i] = rng.Float32()*4 - 2
			b[i] = rng.Float32()*4 - 2
		}
		rawA := encodeRawFloat32(nil, a)
		rawB := encodeRawFloat32(nil, b)
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

func TestAccumulatorHeadroom1024D(t *testing.T) {
	n := int(Dim1024)
	// int8/uint8 worst case: every diff is 255.
	a := make([]int8, n)
	b := make([]int8, n)
	for i := range a {
		a[i], b[i] = -128, 127
	}
	if want, got := maxDistInt8U8, SquaredL2Int8(a, b); got != want {
		t.Fatalf("int8 headroom = %v, want %v", got, want)
	}
	au := make([]uint8, n)
	bu := make([]uint8, n)
	for i := range au {
		au[i], bu[i] = 0, 255
	}
	if want, got := maxDistInt8U8, SquaredL2Uint8(au, bu); got != want {
		t.Fatalf("uint8 headroom = %v, want %v", got, want)
	}
	// int16/uint16 worst case.
	ai := make([]int16, n)
	bi := make([]int16, n)
	for i := range ai {
		ai[i], bi[i] = -32768, 32767
	}
	if want, got := maxDistInt16U16, SquaredL2Int16(ai, bi); got != want {
		t.Fatalf("int16 headroom = %v, want %v", got, want)
	}
	if got := SquaredL2Int16(ai, bi); got >= 1<<53 {
		t.Fatalf("int16 headroom %v exceeds float64 exact-integer range", got)
	}
	ay := make([]uint16, n)
	by := make([]uint16, n)
	for i := range ay {
		ay[i], by[i] = 0, 65535
	}
	if want, got := maxDistInt16U16, SquaredL2Uint16(ay, by); got != want {
		t.Fatalf("uint16 headroom = %v, want %v", got, want)
	}
}

func TestEncodeScoreMonotonicAndSaturating(t *testing.T) {
	distances := []float64{0, 1, 2.9, 100, 1e6, float64(math.MaxUint32) - 1, float64(math.MaxUint32), float64(math.MaxUint32) + 1000, 4.4e12}
	prev := uint32(0)
	for i, d := range distances {
		s := EncodeScore(d)
		if i > 0 && s < prev {
			t.Fatalf("score not monotonic: dist %v -> %v after %v", d, s, prev)
		}
		prev = s
	}
	if got := EncodeScore(0); got != 0 {
		t.Fatalf("zero distance score = %v, want 0", got)
	}
	if got := EncodeScore(-5); got != 0 {
		t.Fatalf("negative distance score = %v, want 0", got)
	}
	if got := EncodeScore(4.4e12); got != math.MaxUint32 {
		t.Fatalf("far int16 distance saturates to %v, want MaxUint32", got)
	}
	if got := EncodeScore(math.NaN()); got != math.MaxUint32 {
		t.Fatalf("NaN score = %v, want MaxUint32", got)
	}
}

func TestResolveDistanceCoversMatrix(t *testing.T) {
	for _, elem := range []ElementType{ElementInt8, ElementUint8, ElementInt16, ElementUint16, ElementFloat32} {
		for _, dims := range []Dimensions{Dim512, Dim768, Dim1024} {
			fn := resolveDistance(Config{Dimensions: dims, ElementType: elem})
			if fn == nil {
				t.Fatalf("no kernel for elem %d dim %d", elem, dims)
			}
			if dot := resolveDot(Config{Dimensions: dims, ElementType: elem}); dot == nil {
				t.Fatalf("no dot kernel for elem %d dim %d", elem, dims)
			}
		}
	}
	if fn := resolveDistance(Config{Dimensions: Dim512, ElementType: ElementInvalid}); fn != nil {
		t.Fatalf("expected nil kernel for invalid element type")
	}
	if fn := resolveDot(Config{Dimensions: Dim512, ElementType: ElementInvalid}); fn != nil {
		t.Fatalf("expected nil dot kernel for invalid element type")
	}
}

func TestConfigVectorBytesTable(t *testing.T) {
	cases := []struct {
		elem ElementType
		dims Dimensions
		want int
	}{
		{ElementInt8, Dim512, 512},
		{ElementUint8, Dim768, 768},
		{ElementInt16, Dim512, 1024},
		{ElementUint16, Dim768, 1536},
		{ElementFloat32, Dim512, 2048},
		{ElementFloat32, Dim768, 3072},
		{ElementFloat32, Dim1024, 4096},
	}
	for _, c := range cases {
		cfg, err := Config{Namespace: testNamespace, Dimensions: c.dims, ElementType: c.elem}.withDefaults()
		if err != nil {
			t.Fatalf("elem %d dim %d: %v", c.elem, c.dims, err)
		}
		if got := cfg.VectorBytes(); got != c.want {
			t.Fatalf("elem %d dim %d: VectorBytes = %d, want %d", c.elem, c.dims, got, c.want)
		}
	}
}
