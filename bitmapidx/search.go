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
// Delta-free operands contribute their raw base without cloning; only
// delta-carrying operands are materialized, and the combination runs as a
// single N-way intersection.
func Intersect(indexes ...*Index) idset.Ref {
	if len(indexes) == 0 {
		return idset.Empty()
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)

	planned := planIntersection(indexes)
	operands := idset.NewOperands(len(planned))
	for _, index := range planned {
		if index == nil || index.closed.Load() {
			return idset.Empty()
		}
		view := index.view.Load()
		if view.hasDeltas() {
			operands.Add(view.effectiveBitmap())
		} else {
			operands.Add(view.base)
		}
	}
	return operands.And()
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
	// Cursors accelerate repeated probes against delta-free bases. They are
	// per-call scratch (never shared) and only used when the view carries no
	// correction layers, so results match view.contains exactly.
	var cursorStack [8]idset.Cursor
	var useCursorStack [8]bool
	var cursors []idset.Cursor
	var useCursor []bool
	if len(views) > len(cursorStack) {
		cursors = make([]idset.Cursor, len(views))
		useCursor = make([]bool, len(views))
	} else {
		cursors = cursorStack[:len(views)]
		useCursor = useCursorStack[:len(views)]
	}
	for i, view := range views {
		if !view.hasDeltas() {
			cursors[i] = view.base.NewCursor()
			useCursor[i] = true
		}
	}
	for _, id := range candidates {
		present := true
		for i, view := range views {
			var ok bool
			if useCursor[i] {
				ok = cursors[i].Contains(id)
			} else {
				ok = view.contains(id)
			}
			if !ok {
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
