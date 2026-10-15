package bitmapidx

import schema "github.com/evanoberholster/imageIdx/schema"

// Cardinality returns the number of distinct values with at least one live
// ImageID in this namespace. It counts a value once regardless of its posting size,
// excludes dictionary values whose last membership was removed, and returns
// zero for a closed namespace. It reads one immutable namespace view without locking
// writers or materializing any postings.
func (i *Index) Cardinality() uint64 {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0
	}
	// A BSI "value" is the number of ValueIDs holding a value: the existence
	// bitmap cardinality, not the count of slice postings.
	if view.namespace.Kind == schema.KindBitmapBSI {
		if exists, ok := view.bsiExists(view.namespace.Namespace); ok {
			return exists.Cardinality()
		}
		return 0
	}
	count := uint64(len(view.base.keys))
	if !view.hasDeltas() {
		return count
	}

	// Only keys touched by a delta can differ from Base. Merge the two sorted
	// delta key lists so each key is adjusted once without copying Base keys.
	var frozen, active []NamespaceKey
	if view.frozen != nil {
		frozen = view.frozen.keys
	}
	if view.active != nil {
		active = view.active.keys
	}
	for f, a := 0, 0; f < len(frozen) || a < len(active); {
		var key NamespaceKey
		switch {
		case a == len(active):
			key = frozen[f]
			f++
		case f == len(frozen):
			key = active[a]
			a++
		default:
			switch order := compareNamespaceKey(frozen[f], active[a]); {
			case order < 0:
				key = frozen[f]
				f++
			case order > 0:
				key = active[a]
				a++
			default:
				key = frozen[f]
				f++
				a++
			}
		}
		_, inBase := view.base.get(key)
		live := view.estimateKeys([]NamespaceKey{key}) != 0
		switch {
		case inBase && !live:
			count--
		case !inBase && live:
			count++
		}
	}
	return count
}

// Cardinality returns the distinct live-value count for one registered
// schema.Namespace. ok is false when the type is not registered; a registered but
// empty or closed namespace reports (0, true).
func (s *Store) Cardinality(t schema.Namespace) (count uint64, ok bool) {
	namespace, ok := s.Lookup(t)
	if !ok {
		return 0, false
	}
	return namespace.Cardinality(), true
}

// Cardinalities returns the distinct live-value count for every registered
// schema.Namespace, including empty namespaces. Each namespace is sampled independently;
// concurrent cross-namespace writes do not produce a store-wide snapshot.
func (s *Store) Cardinalities() map[schema.Namespace]uint64 {
	counts := make(map[schema.Namespace]uint64, len(s.order))
	for _, namespace := range s.order {
		counts[namespace.spec().Namespace] = namespace.Cardinality()
	}
	return counts
}
