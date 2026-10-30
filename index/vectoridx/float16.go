package vectoridx

import "math"

// Float16ToFloat32 converts one IEEE 754 binary16 value to float32.
func Float16ToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x03ff)
	if exp == 0 {
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		e := int32(-14)
		for mant&0x0400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x03ff
		return math.Float32frombits(sign | uint32(e+127)<<23 | mant<<13)
	}
	if exp == 0x1f {
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	}
	return math.Float32frombits(sign | (exp-15+127)<<23 | mant<<13)
}

// Float32ToFloat16 converts one float32 value to IEEE 754 binary16 using
// round-to-nearest-even. Values outside the finite half range become infinity;
// NaN becomes a quiet NaN.
func Float32ToFloat16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int((b>>23)&0xff) - 127
	mant := b & 0x7fffff
	if exp == 128 {
		if mant == 0 {
			return sign | 0x7c00
		}
		return sign | 0x7e00
	}
	halfExp := exp + 15
	if halfExp >= 31 {
		return sign | 0x7c00
	}
	if halfExp <= 0 {
		if halfExp < -10 {
			return sign
		}
		mant |= 1 << 23
		shift := uint32(14 - halfExp)
		return sign | uint16(roundShiftRight(mant, shift))
	}
	rounded := roundShiftRight(mant, 13)
	if rounded == 1<<10 {
		halfExp++
		if halfExp >= 31 {
			return sign | 0x7c00
		}
		rounded = 0
	}
	return sign | uint16(halfExp<<10) | uint16(rounded)
}

func roundShiftRight(v, shift uint32) uint32 {
	if shift == 0 {
		return v
	}
	half := uint32(1) << (shift - 1)
	rem := v & ((uint32(1) << shift) - 1)
	out := v >> shift
	if rem > half || (rem == half && out&1 != 0) {
		out++
	}
	return out
}
