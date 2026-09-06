package roar

// arrayLinearSearchThreshold switches array membership from a branch-light
// linear scan to binary search. It is a runtime policy that benchmarks may
// tune.
const arrayLinearSearchThreshold = 16

// gallopFactor is the size ratio at which an array merge switches from a
// linear walk to galloping binary search over the larger array.
const gallopFactor = 8

// shouldGallop reports whether small is small enough relative to large that
// per-element binary search beats a linear merge.
func shouldGallop(small, large []uint16) bool {
	return len(large) > gallopFactor*len(small)
}

// lowerBoundFrom returns the index of the first element >= target, starting
// the search at from. Successive calls with non-decreasing targets and from
// values are amortized O(1) via exponential search.
func lowerBoundFrom(values []uint16, from int, target uint16) int {
	if from >= len(values) || values[from] >= target {
		return from
	}
	step := 1
	for from+step < len(values) && values[from+step] < target {
		from += step
		step <<= 1
	}
	lo, hi := from, from+step
	if hi > len(values) {
		hi = len(values)
	}
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// arrayContains reports whether a sorted array contains x. The binary path
// is a manual loop to keep Contains inlinable (sort.Search closures do not
// inline).
func arrayContains(values []uint16, x uint16) bool {
	if len(values) <= arrayLinearSearchThreshold {
		for _, value := range values {
			if value == x {
				return true
			}
			if value > x {
				return false
			}
		}
		return false
	}
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] < x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(values) && values[lo] == x
}

// intersectArrays writes the sorted intersection of a and b to *dst.
func intersectArrays(dst *[]uint16, a, b []uint16) {
	if len(a) > len(b) {
		a, b = b, a
	}
	out := (*dst)[:0]
	if shouldGallop(a, b) {
		pos := 0
		for _, value := range a {
			pos = lowerBoundFrom(b, pos, value)
			if pos < len(b) && b[pos] == value {
				out = append(out, value)
			}
		}
		*dst = out
		return
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	*dst = out
}

// intersectArraysCount counts the sorted intersection of a and b.
func intersectArraysCount(a, b []uint16) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	if shouldGallop(a, b) {
		count := 0
		pos := 0
		for _, value := range a {
			pos = lowerBoundFrom(b, pos, value)
			if pos < len(b) && b[pos] == value {
				count++
			}
		}
		return count
	}
	count := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			count++
			i++
			j++
		}
	}
	return count
}

// unionArrays writes the sorted union of a and b to *dst.
func unionArrays(dst *[]uint16, a, b []uint16) {
	out := (*dst)[:0]
	switch {
	case shouldGallop(a, b):
		unionGallop(&out, a, b)
	case shouldGallop(b, a):
		unionGallop(&out, b, a)
	default:
		i, j := 0, 0
		for i < len(a) && j < len(b) {
			switch {
			case a[i] < b[j]:
				out = append(out, a[i])
				i++
			case a[i] > b[j]:
				out = append(out, b[j])
				j++
			default:
				out = append(out, a[i])
				i++
				j++
			}
		}
		out = append(out, a[i:]...)
		out = append(out, b[j:]...)
	}
	*dst = out
}

// unionGallop writes the union of small and large, where large is at least
// gallopFactor times small.
func unionGallop(dst *[]uint16, small, large []uint16) {
	out := *dst
	pos := 0
	for _, value := range small {
		g := lowerBoundFrom(large, pos, value)
		out = append(out, large[pos:g]...)
		if g < len(large) && large[g] == value {
			pos = g + 1
		} else {
			pos = g
		}
		out = append(out, value)
	}
	*dst = append(out, large[pos:]...)
}

// unionArraysCount counts the sorted union of a and b without materializing
// it.
func unionArraysCount(a, b []uint16) int {
	return len(a) + len(b) - intersectArraysCount(a, b)
}

// differenceArrays writes a minus b to *dst.
func differenceArrays(dst *[]uint16, a, b []uint16) {
	out := (*dst)[:0]
	if shouldGallop(b, a) {
		pos := 0
		for _, value := range a {
			pos = lowerBoundFrom(b, pos, value)
			if pos >= len(b) || b[pos] != value {
				out = append(out, value)
			}
		}
		*dst = out
		return
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			j++
		default:
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	*dst = out
}

// differenceArraysCount counts a minus b without materializing it.
func differenceArraysCount(a, b []uint16) int {
	return len(a) - intersectArraysCount(a, b)
}
