package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/idset"
	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

type readToken struct {
	readers *indexutil.ReaderTracker
	token   uint32
}

// Intersect returns the logical intersection of the supplied immutable
// snapshots. Each operand's Active/Frozen corrections are applied first.
// When no operand carries deltas the intersection runs directly over the raw
// bases with a single clone.
func Intersect(indexes ...*Index) idset.Ref {
	if len(indexes) == 0 {
		return idset.Empty()
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)

	planned := planIntersection(indexes)
	if allDeltaFree(planned) {
		operands := idset.NewOperands(len(planned))
		for _, index := range planned {
			if index == nil || index.closed.Load() {
				return idset.Empty()
			}
			operands.Add(index.view.Load().base)
		}
		return operands.And()
	}

	var result idset.Builder
	for position, index := range planned {
		if index == nil || index.closed.Load() {
			return idset.Empty()
		}
		operand := index.view.Load().effectiveBuilder()
		if position == 0 {
			result = operand
			continue
		}
		result.And(operand.Freeze())
	}
	result.Cleanup()
	return result.Freeze()
}

// Union returns the logical union. Removals remain operand-aware because each
// operand is corrected independently before the union is evaluated. Delta-free
// operands are added without cloning their base.
func Union(indexes ...*Index) idset.Ref {
	if len(indexes) == 0 {
		return idset.Empty()
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)

	operands := idset.NewOperands(len(indexes))
	for _, index := range indexes {
		if index == nil || index.closed.Load() {
			continue
		}
		view := index.view.Load()
		if view.hasDeltas() {
			operands.Add(view.effectiveBitmap())
		} else {
			operands.Add(view.base)
		}
	}
	return operands.Or()
}

// FilterCandidates appends candidates present in every index to dst. It enters
// each distinct internal reader tracker once and supports dst reusing
// candidates' backing storage, allowing a scheduler to filter an existing
// candidate slice in place.
func FilterCandidates(dst, candidates []ImageID, indexes ...*Index) []ImageID {
	if len(indexes) == 0 {
		return append(dst, candidates...)
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	result := filterCandidatesGuarded(dst, candidates, indexes)
	releaseReaders(tokens)
	return result
}

func filterCandidatesGuarded(dst, candidates []ImageID, indexes []*Index) []ImageID {
	var plannedStack [8]*Index
	planned := plannedStack[:0]
	if len(indexes) > len(plannedStack) {
		planned = make([]*Index, 0, len(indexes))
	}
	planned = planIntersectionInto(planned, indexes)
	var viewStack [8]*indexView
	views := viewStack[:0]
	if len(planned) > len(viewStack) {
		views = make([]*indexView, 0, len(planned))
	}
	for _, index := range planned {
		if index == nil || index.closed.Load() {
			return dst
		}
		views = append(views, index.view.Load())
	}
	for _, id := range candidates {
		present := true
		for _, view := range views {
			if !view.contains(id) {
				present = false
				break
			}
		}
		if present {
			dst = append(dst, id)
		}
	}
	return dst
}

// allDeltaFree reports whether every index currently publishes a view with no
// correction layers, which lets Intersect operate on the raw Bases directly.
func allDeltaFree(indexes []*Index) bool {
	for _, index := range indexes {
		if index == nil {
			return false
		}
		if index.view.Load().hasDeltas() {
			return false
		}
	}
	return true
}

// acquireReaders registers with each distinct tracker, deduplicating privately
// shared store trackers without allocating for up to eight indexes.
func acquireReaders(dst []readToken, indexes []*Index) []readToken {
	for _, index := range indexes {
		if index == nil || index.readers == nil {
			continue
		}
		seen := false
		for _, acquired := range dst {
			if acquired.readers == index.readers {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		dst = append(dst, readToken{readers: index.readers, token: index.readers.Acquire()})
	}
	return dst
}

// releaseReaders unregisters in reverse acquisition order.
func releaseReaders(tokens []readToken) {
	for i := len(tokens) - 1; i >= 0; i-- {
		tokens[i].readers.Release(tokens[i].token)
	}
}
