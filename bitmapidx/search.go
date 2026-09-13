package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/roar"
)

type readToken struct {
	readers *indexutil.ReaderTracker
	token   uint32
}

// And returns the logical intersection of the supplied immutable
// snapshots. Each operand's Active/Frozen corrections are applied first.
// Delta-free operands contribute their raw base without cloning; only
// delta-carrying operands are materialized, and the combination runs as a
// single N-way intersection. A nil index is an empty predicate and empties
// the conjunction.
func And(indexes ...*Index) (result roar.Bitmap) {
	if len(indexes) == 0 {
		return roar.Bitmap{}
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)
	defer func() { result = ownStoreResult(result, indexes) }()

	if len(indexes) <= 2 {
		a, ok := andOperand(indexes, 0)
		if !ok {
			return roar.Bitmap{}
		}
		if len(indexes) == 1 {
			return a
		}
		b, ok := andOperand(indexes, 1)
		if !ok {
			return roar.Bitmap{}
		}
		return roar.And(a, b)
	}

	planned := planAnd(indexes)
	operands := make([]roar.Bitmap, 0, len(planned))
	for _, index := range planned {
		if index == nil || index.closed.Load() {
			return roar.Bitmap{}
		}
		view := index.view.Load()
		if view.hasDeltas() {
			operands = append(operands, view.effectiveBitmap())
		} else {
			operands = append(operands, view.base)
		}
	}
	switch len(operands) {
	case 0:
		return roar.Bitmap{}
	case 1:
		return operands[0]
	default:
		return roar.AndMany(operands...)
	}
}

// andOperand materializes one AND operand: the shared base for delta-free
// indexes, otherwise the corrected bitmap. ok is false for a missing or
// closed index, which empties the conjunction.
func andOperand(indexes []*Index, at int) (roar.Bitmap, bool) {
	index := indexes[at]
	if index == nil || index.closed.Load() {
		return roar.Bitmap{}, false
	}
	view := index.view.Load()
	if view.hasDeltas() {
		return view.effectiveBitmap(), true
	}
	return view.base, true
}

// Or returns the logical union. Removals remain operand-aware because each
// operand is corrected independently before the union is evaluated. Delta-free
// operands are added without cloning their base. Nil indexes are skipped as
// empty predicates.
func Or(indexes ...*Index) (result roar.Bitmap) {
	if len(indexes) == 0 {
		return roar.Bitmap{}
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)
	defer func() { result = ownStoreResult(result, indexes) }()

	if len(indexes) <= 2 {
		var first, second roar.Bitmap
		live := 0
		for _, index := range indexes {
			if index == nil || index.closed.Load() {
				continue
			}
			view := index.view.Load()
			var operand roar.Bitmap
			if view.hasDeltas() {
				operand = view.effectiveBitmap()
			} else {
				operand = view.base
			}
			if live == 0 {
				first = operand
			} else {
				second = operand
			}
			live++
		}
		switch live {
		case 0:
			return roar.Bitmap{}
		case 1:
			return first
		default:
			return roar.Or(first, second)
		}
	}

	operands := make([]roar.Bitmap, 0, len(indexes))
	for _, index := range indexes {
		if index == nil || index.closed.Load() {
			continue
		}
		view := index.view.Load()
		if view.hasDeltas() {
			operands = append(operands, view.effectiveBitmap())
		} else {
			operands = append(operands, view.base)
		}
	}
	switch len(operands) {
	case 0:
		return roar.Bitmap{}
	case 1:
		return operands[0]
	default:
		return roar.OrMany(operands...)
	}
}

// Count returns the number of images present in every index without
// materializing the intersection. Delta-free operands count directly from
// container headers without allocating (up to eight indexes); when any
// operand carries corrections it falls back to And, which evaluates them
// per ID. A nil index counts zero.
func Count(indexes ...*Index) uint64 {
	if len(indexes) == 0 {
		return 0
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	defer releaseReaders(tokens)

	var planStack [8]*Index
	planned := planStack[:0]
	if len(indexes) > len(planStack) {
		planned = make([]*Index, 0, len(indexes))
	}
	planned = planAndInto(planned, indexes)
	var baseStack [8]roar.Bitmap
	bases := baseStack[:0]
	if len(planned) > len(baseStack) {
		bases = make([]roar.Bitmap, 0, len(planned))
	}
	for _, index := range planned {
		if index == nil || index.closed.Load() {
			return 0
		}
		view := index.view.Load()
		if view.hasDeltas() {
			return And(planned...).Cardinality()
		}
		bases = append(bases, view.base)
	}
	return roar.AndManyCardinality(bases...)
}

// Filter appends candidates present in every index to dst. It enters
// each distinct internal reader tracker once and supports dst reusing
// candidates' backing storage, allowing a scheduler to filter an existing
// candidate slice in place. A nil index matches nothing, so no candidates
// are appended once one is seen.
func Filter(dst, candidates []ImageID, indexes ...*Index) []ImageID {
	if len(indexes) == 0 {
		return append(dst, candidates...)
	}
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], indexes)
	result := filterGuarded(dst, candidates, indexes)
	releaseReaders(tokens)
	return result
}

func filterGuarded(dst, candidates []ImageID, indexes []*Index) []ImageID {
	var plannedStack [8]*Index
	planned := plannedStack[:0]
	if len(indexes) > len(plannedStack) {
		planned = make([]*Index, 0, len(indexes))
	}
	planned = planAndInto(planned, indexes)
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
	var cursorStack [8]roar.Cursor
	var useCursorStack [8]bool
	var cursors []roar.Cursor
	var useCursor []bool
	if len(views) > len(cursorStack) {
		cursors = make([]roar.Cursor, len(views))
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
