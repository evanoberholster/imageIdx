package vectoridx

// Query is a typed vector query accepted by the generic Search, SearchInto,
// ExactSearch, and SearchCandidates methods. Set exactly one slice field; that
// field selects the element type and must match the index configuration. Every
// query length must equal Config.Dimensions.
//
// The typed SearchFloat32/SearchInt8/... methods remain the shortest path when
// the element type is known at the call site. Query is useful for callers that
// select a vector namespace at runtime, including the search package.
type Query struct {
	Int8   []int8
	Uint8  []uint8
	Int16  []int16
	Uint16 []uint16
	// Float16 stores IEEE binary16 bits, matching ElementFloat16.
	Float16 []uint16
	Float32 []float32
}

// Element reports the query element type. It returns false when zero or more
// than one slice field is set.
func (q Query) Element() (ElementType, bool) {
	elem, _, ok := q.element()
	return elem, ok
}

func (q Query) element() (ElementType, int, bool) {
	count := 0
	var elem ElementType
	var n int
	if q.Int8 != nil {
		elem, n = ElementInt8, len(q.Int8)
		count++
	}
	if q.Uint8 != nil {
		elem, n = ElementUint8, len(q.Uint8)
		count++
	}
	if q.Int16 != nil {
		elem, n = ElementInt16, len(q.Int16)
		count++
	}
	if q.Uint16 != nil {
		elem, n = ElementUint16, len(q.Uint16)
		count++
	}
	if q.Float16 != nil {
		elem, n = ElementFloat16, len(q.Float16)
		count++
	}
	if q.Float32 != nil {
		elem, n = ElementFloat32, len(q.Float32)
		count++
	}
	if count != 1 {
		return 0, 0, false
	}
	return elem, n, true
}

func (q Query) validate(cfg Config) (ElementType, error) {
	elem, n, ok := q.element()
	if !ok || elem != cfg.ElementType || n != int(cfg.Dimensions) {
		return 0, ErrInvalidVector
	}
	return elem, nil
}

// encode writes the validated query into dst, reusing its capacity.
func (q Query) encode(dst []byte) []byte {
	switch {
	case q.Int8 != nil:
		return encodeRawInt8(dst, q.Int8)
	case q.Uint8 != nil:
		return encodeRawUint8(dst, q.Uint8)
	case q.Int16 != nil:
		return encodeRawInt16(dst, q.Int16)
	case q.Uint16 != nil:
		return encodeRawUint16(dst, q.Uint16)
	case q.Float16 != nil:
		return encodeRawFloat16(dst, q.Float16)
	case q.Float32 != nil:
		return encodeRawFloat32(dst, q.Float32)
	default:
		return dst
	}
}

func normalizeFilterHint(hint int) int {
	if hint == 0 {
		return UnknownFilterHint
	}
	return hint
}
