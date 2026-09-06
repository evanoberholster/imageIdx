package bitmapkernel

// Dispatch variables select the active backend. They default to the scalar
// reference and are replaced during initialization by a SIMD backend when one
// is compiled in and supported by the CPU. Hot loops call the wrappers below,
// never these variables directly.

var (
	andImpl               = andScalar
	orImpl                = orScalar
	andNotImpl            = andNotScalar
	intersectsImpl        = intersectsScalar
	andCardinalityImpl    = andCardinalityScalar
	orCardinalityImpl     = orCardinalityScalar
	andNotCardinalityImpl = andNotCardinalityScalar
	cardinalityImpl       = cardinalityScalar
	andManyImpl           = andManyScalar
	applyImpl             = applyScalar
)

// And writes a & b to dst and returns the exact cardinality of the result.
func And(dst, a, b []uint64) uint32 { return andImpl(dst, a, b) }

// Or writes a | b to dst and returns the exact cardinality of the result.
func Or(dst, a, b []uint64) uint32 { return orImpl(dst, a, b) }

// AndNot writes a &^ b to dst and returns the exact cardinality of the result.
func AndNot(dst, a, b []uint64) uint32 { return andNotImpl(dst, a, b) }

// Intersects reports whether a and b share any set bit, exiting early.
func Intersects(a, b []uint64) bool { return intersectsImpl(a, b) }

// AndCardinality returns the cardinality of a & b without materializing it.
func AndCardinality(a, b []uint64) uint64 { return andCardinalityImpl(a, b) }

// OrCardinality returns the cardinality of a | b without materializing it.
func OrCardinality(a, b []uint64) uint64 { return orCardinalityImpl(a, b) }

// AndNotCardinality returns the cardinality of a &^ b without materializing
// it.
func AndNotCardinality(a, b []uint64) uint64 { return andNotCardinalityImpl(a, b) }

// Cardinality returns the number of set bits in w.
func Cardinality(w []uint64) uint64 { return cardinalityImpl(w) }

// AndMany writes the N-way intersection of inputs to dst and returns its
// cardinality. len(dst) and every len(inputs[i]) must be equal.
func AndMany(dst []uint64, inputs ...[]uint64) uint32 {
	return andManyImpl(dst, inputs...)
}

// Apply writes (base | add) &^ remove to dst and returns its cardinality.
func Apply(dst, base, add, remove []uint64) uint32 {
	return applyImpl(dst, base, add, remove)
}
