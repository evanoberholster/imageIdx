package bitmapkernel

// fixedBitmapWords is the 65,536-bit dense container size used by roar.
const fixedBitmapWords = 1024

// Dispatch variables select the active backend. They default to the scalar
// reference and are replaced during initialization by a SIMD backend when one
// is compiled in and supported by the CPU. Hot loops call the wrappers below,
// never these variables directly.

var (
	andKernel               = andScalar
	orKernel                = orScalar
	andNotKernel            = andNotScalar
	intersectsKernel        = intersectsScalar
	andCardinalityKernel    = andCardinalityScalar
	orCardinalityKernel     = orCardinalityScalar
	andNotCardinalityKernel = andNotCardinalityScalar
	cardinalityKernel       = cardinalityScalar
	andManyKernel           = andManyScalar
	andMany3Kernel          = andMany3Scalar
	andMany4Kernel          = andMany4Scalar
	applyKernel             = applyScalar
)

// And writes a & b to dst and returns the exact cardinality of the result.
func And(dst, a, b []uint64) uint32 { return andKernel(dst, a, b) }

// Or writes a | b to dst and returns the exact cardinality of the result.
func Or(dst, a, b []uint64) uint32 { return orKernel(dst, a, b) }

// AndNot writes a &^ b to dst and returns the exact cardinality of the result.
func AndNot(dst, a, b []uint64) uint32 { return andNotKernel(dst, a, b) }

// Intersects reports whether a and b share any set bit, exiting early.
func Intersects(a, b []uint64) bool { return intersectsKernel(a, b) }

// AndCardinality returns the cardinality of a & b without materializing it.
func AndCardinality(a, b []uint64) uint64 { return andCardinalityKernel(a, b) }

// OrCardinality returns the cardinality of a | b without materializing it.
func OrCardinality(a, b []uint64) uint64 { return orCardinalityKernel(a, b) }

// AndNotCardinality returns the cardinality of a &^ b without materializing
// it.
func AndNotCardinality(a, b []uint64) uint64 { return andNotCardinalityKernel(a, b) }

// Cardinality returns the number of set bits in w.
func Cardinality(w []uint64) uint64 { return cardinalityKernel(w) }

// AndMany writes the N-way intersection of inputs to dst and returns its
// cardinality. len(dst) and every len(inputs[i]) must be equal.
func AndMany(dst []uint64, inputs ...[]uint64) uint32 {
	switch len(inputs) {
	case 2:
		return andKernel(dst, inputs[0], inputs[1])
	case 3:
		return andMany3Kernel(dst, inputs[0], inputs[1], inputs[2])
	case 4:
		return andMany4Kernel(dst, inputs[0], inputs[1], inputs[2], inputs[3])
	}
	return andManyKernel(dst, inputs...)
}

// AndMany3 writes a & b & c to dst without constructing a variadic input
// slice. It is the allocation-free fast path for the common three-way case.
func AndMany3(dst, a, b, c []uint64) uint32 {
	return andMany3Kernel(dst, a, b, c)
}

// AndMany4 writes a & b & c & d to dst without constructing a variadic input
// slice. It is the allocation-free fast path for the common four-way case.
func AndMany4(dst, a, b, c, d []uint64) uint32 {
	return andMany4Kernel(dst, a, b, c, d)
}

// Apply writes (base | add) &^ remove to dst and returns its cardinality.
func Apply(dst, base, add, remove []uint64) uint32 {
	return applyKernel(dst, base, add, remove)
}
