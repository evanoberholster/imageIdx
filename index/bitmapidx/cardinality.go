package bitmapidx

// Cardinality returns the number of distinct values with at least one live
// ImageID in this field. It counts a value once regardless of its posting size,
// excludes dictionary values whose last membership was removed, and returns
// zero for a closed field. It reads one immutable field view without locking
// writers or materializing any postings.
func (i *FieldIndex) Cardinality() uint64 {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0
	}
	count := uint64(len(view.base.keys))
	if !view.hasDeltas() {
		return count
	}

	// Only keys touched by a delta can differ from Base. Merge the two sorted
	// delta key lists so each key is adjusted once without copying Base keys.
	var frozen, active []FieldKey
	if view.frozen != nil {
		frozen = view.frozen.keys
	}
	if view.active != nil {
		active = view.active.keys
	}
	for f, a := 0, 0; f < len(frozen) || a < len(active); {
		var key FieldKey
		switch {
		case a == len(active):
			key = frozen[f]
			f++
		case f == len(frozen):
			key = active[a]
			a++
		default:
			switch order := compareFieldKey(frozen[f], active[a]); {
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
		live := view.estimateKeys([]FieldKey{key}) != 0
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
// IndexType. ok is false when the type is not registered; a registered but
// empty or closed field reports (0, true).
func (s *Index) Cardinality(t IndexType) (count uint64, ok bool) {
	field, ok := s.Lookup(t)
	if !ok {
		return 0, false
	}
	return field.Cardinality(), true
}

// Cardinalities returns the distinct live-value count for every registered
// IndexType, including empty fields. Each field is sampled independently;
// concurrent cross-field writes do not produce a store-wide snapshot.
func (s *Index) Cardinalities() map[IndexType]uint64 {
	counts := make(map[IndexType]uint64, len(s.order))
	for _, field := range s.order {
		counts[field.field().Type] = field.Cardinality()
	}
	return counts
}
