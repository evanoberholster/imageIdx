package rankkernel

// Dispatch seam: bulk loops call the variables below, never the scalar
// bodies directly. Future AVX2/NEON backends replace the bulk variables at
// init time after proving equivalence against the scalar reference and a
// benchmark win. Single-value helpers always use the scalar reference.

var (
	encodeDistancesKernel = encodeDistancesScalar
	quantizeCosinesKernel = quantizeCosinesScalar
)

// EncodeDistance is the scalar reference for one native distance.
func EncodeDistance(d uint32) uint16 { return encodeDistanceScalar(d) }

// QuantizeCosine is the scalar reference for one cosine similarity.
func QuantizeCosine(sim float32) uint16 { return quantizeCosineScalar(sim) }

// EncodeDistances writes one rank per native distance. len(dst) must be at
// least len(src). Higher rank is better; 0 never appears except via explicit
// missing handling by the caller (this kernel maps 0 distance to 65535).
func EncodeDistances(dst []uint16, src []uint32) {
	encodeDistancesKernel(dst, src)
}

// QuantizeCosines writes one rank per cosine similarity. NaN maps to 0.
func QuantizeCosines(dst []uint16, src []float32) {
	quantizeCosinesKernel(dst, src)
}
