// Package kernels implements the scalar and SIMD distance kernels used by
// vectoridx. Element families live in separate files; this file owns the
// element type and the dispatch from a raw byte vector to a kernel.
package kernels

// Element identifies one vector element type. Values are independent of the
// public vectoridx.ElementType values; callers translate at the boundary.
type Element uint8

const (
	Int8 Element = iota
	Uint8
	Int16
	Uint16
	Float32
	Float16
)

// L2 returns the raw squared-L2 kernel for e and dimensions n, selecting a
// SIMD implementation when the host supports one. Unknown elements return nil.
func L2(e Element, n int) Func {
	switch e {
	case Int8:
		if simdInt8Supported {
			return simdL2Int8Bytes
		}
		return func(a, b []byte) float64 {
			return SquaredL2Int8(BytesAsInt8(a, n), BytesAsInt8(b, n))
		}
	case Uint8:
		if simdInt8Supported {
			return simdL2Uint8Bytes
		}
		return func(a, b []byte) float64 {
			return SquaredL2Uint8(BytesAsUint8(a, n), BytesAsUint8(b, n))
		}
	case Int16:
		return func(a, b []byte) float64 {
			return SquaredL2Int16(BytesAsInt16(a, n), BytesAsInt16(b, n))
		}
	case Uint16:
		return func(a, b []byte) float64 {
			return SquaredL2Uint16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	case Float32:
		if simdSupported {
			return simdL2Float32Bytes
		}
		return func(a, b []byte) float64 {
			return SquaredL2Float32(BytesAsFloat32(a, n), BytesAsFloat32(b, n))
		}
	case Float16:
		if simdFloat16Supported {
			return simdL2Float16Bytes
		}
		return func(a, b []byte) float64 {
			return SquaredL2Float16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	default:
		return nil
	}
}

// Dot returns the raw positive-dot kernel for e and dimensions n.
func Dot(e Element, n int) Func {
	switch e {
	case Int8:
		if simdInt8Supported {
			return simdDotInt8Bytes
		}
		return func(a, b []byte) float64 {
			return DotInt8(BytesAsInt8(a, n), BytesAsInt8(b, n))
		}
	case Uint8:
		if simdInt8Supported {
			return simdDotUint8Bytes
		}
		return func(a, b []byte) float64 {
			return DotUint8(BytesAsUint8(a, n), BytesAsUint8(b, n))
		}
	case Int16:
		return func(a, b []byte) float64 {
			return DotInt16(BytesAsInt16(a, n), BytesAsInt16(b, n))
		}
	case Uint16:
		return func(a, b []byte) float64 {
			return DotUint16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	case Float32:
		if simdSupported {
			return simdDotFloat32Bytes
		}
		return func(a, b []byte) float64 {
			return DotFloat32(BytesAsFloat32(a, n), BytesAsFloat32(b, n))
		}
	case Float16:
		if simdFloat16Supported {
			return simdDotFloat16Bytes
		}
		return func(a, b []byte) float64 {
			return DotFloat16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	default:
		return nil
	}
}

// NegDot returns the raw negative-dot kernel for e and dimensions n.
func NegDot(e Element, n int) Func {
	switch e {
	case Int8:
		if simdInt8Supported {
			return func(a, b []byte) float64 { return -simdDotInt8Bytes(a, b) }
		}
		return func(a, b []byte) float64 {
			return -DotInt8(BytesAsInt8(a, n), BytesAsInt8(b, n))
		}
	case Uint8:
		if simdInt8Supported {
			return func(a, b []byte) float64 { return -simdDotUint8Bytes(a, b) }
		}
		return func(a, b []byte) float64 {
			return -DotUint8(BytesAsUint8(a, n), BytesAsUint8(b, n))
		}
	case Int16:
		return func(a, b []byte) float64 {
			return -DotInt16(BytesAsInt16(a, n), BytesAsInt16(b, n))
		}
	case Uint16:
		return func(a, b []byte) float64 {
			return -DotUint16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	case Float32:
		if simdSupported {
			return func(a, b []byte) float64 { return -simdDotFloat32Bytes(a, b) }
		}
		return func(a, b []byte) float64 {
			return -DotFloat32(BytesAsFloat32(a, n), BytesAsFloat32(b, n))
		}
	case Float16:
		if simdFloat16Supported {
			return func(a, b []byte) float64 { return -simdDotFloat16Bytes(a, b) }
		}
		return func(a, b []byte) float64 {
			return -DotFloat16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	default:
		return nil
	}
}

// Cosine returns the raw scalar cosine kernel for e and dimensions n.
func Cosine(e Element, n int) Func {
	switch e {
	case Int8:
		return func(a, b []byte) float64 {
			return CosineInt8(BytesAsInt8(a, n), BytesAsInt8(b, n))
		}
	case Uint8:
		return func(a, b []byte) float64 {
			return CosineUint8(BytesAsUint8(a, n), BytesAsUint8(b, n))
		}
	case Int16:
		return func(a, b []byte) float64 {
			return CosineInt16(BytesAsInt16(a, n), BytesAsInt16(b, n))
		}
	case Uint16:
		return func(a, b []byte) float64 {
			return CosineUint16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	case Float32:
		return func(a, b []byte) float64 {
			return CosineFloat32(BytesAsFloat32(a, n), BytesAsFloat32(b, n))
		}
	case Float16:
		return func(a, b []byte) float64 {
			return CosineFloat16(BytesAsUint16(a, n), BytesAsUint16(b, n))
		}
	default:
		return nil
	}
}

// SelfDot returns |q|^2 for a raw query vector.
func SelfDot(e Element, q []byte, n int) float64 {
	switch e {
	case Int8:
		v := BytesAsInt8(q, n)
		return DotInt8(v, v)
	case Uint8:
		v := BytesAsUint8(q, n)
		return DotUint8(v, v)
	case Int16:
		v := BytesAsInt16(q, n)
		return DotInt16(v, v)
	case Uint16:
		v := BytesAsUint16(q, n)
		return DotUint16(v, v)
	case Float32:
		v := BytesAsFloat32(q, n)
		return DotFloat32(v, v)
	case Float16:
		v := BytesAsUint16(q, n)
		return DotFloat16(v, v)
	default:
		return 0
	}
}
