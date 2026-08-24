package roar

import "sort"

// arrayLinearSearchThreshold switches array membership from a branch-light
// linear scan to binary search. It is a runtime policy that benchmarks may
// tune.
const arrayLinearSearchThreshold = 16

// arrayContains reports whether a sorted array contains x.
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
	index := sort.Search(len(values), func(i int) bool {
		return values[i] >= x
	})
	return index < len(values) && values[index] == x
}

// intersectArrays writes the sorted intersection of a and b into dst and
// returns it. dst must have capacity for min(len(a), len(b)).
func intersectArrays(dst, a, b []uint16) []uint16 {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			dst = append(dst, a[i])
			i++
			j++
		}
	}
	return dst
}

// intersectArraysCount counts the sorted intersection of a and b without
// materializing it.
func intersectArraysCount(a, b []uint16) int {
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

// unionArrays writes the sorted union of a and b into dst and returns it.
// dst must have capacity for len(a)+len(b).
func unionArrays(dst, a, b []uint16) []uint16 {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			dst = append(dst, a[i])
			i++
		case a[i] > b[j]:
			dst = append(dst, b[j])
			j++
		default:
			dst = append(dst, a[i])
			i++
			j++
		}
	}
	dst = append(dst, a[i:]...)
	dst = append(dst, b[j:]...)
	return dst
}

// unionArraysCount counts the sorted union of a and b without materializing
// it.
func unionArraysCount(a, b []uint16) int {
	count := 0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			i++
			j++
		}
		count++
	}
	count += len(a) - i
	count += len(b) - j
	return count
}

// differenceArrays writes a minus b into dst and returns it. dst must have
// capacity for len(a).
func differenceArrays(dst, a, b []uint16) []uint16 {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			dst = append(dst, a[i])
			i++
		case a[i] > b[j]:
			j++
		default:
			i++
			j++
		}
	}
	dst = append(dst, a[i:]...)
	return dst
}

// differenceArraysCount counts a minus b without materializing it.
func differenceArraysCount(a, b []uint16) int {
	count := 0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			count++
			i++
		case a[i] > b[j]:
			j++
		default:
			i++
			j++
		}
	}
	count += len(a) - i
	return count
}
