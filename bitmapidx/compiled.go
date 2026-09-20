package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/roar"
)

// Query is a compiled, reusable field conjunction. Values are
// canonicalized, hashed, deduplicated, and dictionary-resolved once at
// Compile time; each execution reads the index's current published view,
// so writes made after compiling are visible without recompiling. A Query
// is safe for concurrent use and holds no query scratch: execution uses
// pooled buffers internally. The zero value is valid and matches nothing.
type Query struct {
	index    *FieldIndex
	keys     []FieldKey
	bases    []FieldKey
	canon    [][]byte
	dictGen  uint64
	complete bool
}

// Compile resolves values to a reusable Query with AND semantics:
// execution matches images holding every value. An empty value list
// compiles to a Query that matches nothing, mirroring MatchValues, as does
// compiling against a closed index. Values absent from the dictionary keep
// their base hash and canonical bytes and resolve lazily at execution, so
// images added after compiling are visible without recompiling.
func (i *FieldIndex) Compile(values ...Value) (Query, error) {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	// A Query always keeps its index so execution stays safe; on a closed
	// index it simply resolves to nothing.
	q := Query{index: i}
	if i.closed.Load() {
		return q, nil
	}
	keys, bases, canon, ok, err := i.resolveValues(view, values, true)
	if err != nil {
		return Query{}, err
	}
	q.bases, q.canon = bases, canon
	if ok {
		q.keys, q.dictGen, q.complete = keys, view.dictGen, true
	}
	return q, nil
}

// CompileText analyzes query text once and compiles the resulting tokens
// with AND semantics. Execution needs no analyzer and allocates no
// tokens. A query producing no tokens compiles to a Query that matches
// nothing.
func (i *FieldIndex) CompileText(query string) (Query, error) {
	if i.Field().Kind != KindText {
		return Query{}, ErrFieldMismatch
	}
	tokens, err := i.analyzer.TokenSet(query, nil)
	if err != nil {
		return Query{}, err
	}
	if len(tokens) == 0 {
		return Query{}, nil
	}
	values := make([]Value, 0, len(tokens))
	for _, text := range tokens {
		values = append(values, Value{Kind: KindText, Bytes: []byte(text)})
	}
	return i.Compile(values...)
}

// resolve returns the execution keys for a view. Fully resolved queries
// reuse their keys while the dictionary generation is unchanged; otherwise
// every value re-resolves against the current view by base hash plus
// canonical bytes. ok is false when a hashed value no longer resolves,
// which empties the conjunction. Continuous values carry no canonical
// bytes and pass through directly.
func (q Query) resolve(view *fieldView) ([]FieldKey, bool) {
	if len(q.bases) == 0 {
		return nil, true
	}
	if q.complete && view.dictGen == q.dictGen {
		return q.keys, true
	}
	keys := make([]FieldKey, 0, len(q.bases))
	for index, base := range q.bases {
		canonical := q.canon[index]
		if canonical == nil {
			keys = append(keys, base)
			continue
		}
		baseKey := FieldKey{Type: base.Type, Hash: base.Hash}
		key, ok := view.dict.resolve(baseKey, canonical)
		if !ok {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

// Match returns the images holding every compiled value, sorted and
// unique. Store-owned results are detached from mmap state.
func (q Query) Match() (result roar.Bitmap, resultErr error) {
	if q.index == nil {
		return roar.Bitmap{}, nil
	}
	i := q.index
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	defer i.ownResult(&result)
	view := i.view.Load()
	if i.closed.Load() {
		return roar.Bitmap{}, nil
	}
	keys, ok := q.resolve(view)
	if !ok {
		return roar.Bitmap{}, nil
	}
	return view.matchResolved(keys), nil
}

// Count returns the number of images holding every compiled value
// without materializing the intersection when possible.
func (q Query) Count() (uint64, error) {
	if q.index == nil {
		return 0, nil
	}
	i := q.index
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	keys, ok := q.resolve(view)
	if !ok {
		return 0, nil
	}
	postings := acquirePostings(len(keys))
	defer releasePostings(postings)
	for _, key := range keys {
		p, ok := view.effective(key)
		if !ok {
			return 0, nil
		}
		*postings = append(*postings, p)
	}
	switch len(*postings) {
	case 0:
		return 0, nil
	case 1:
		return (*postings)[0].cardinality(), nil
	default:
		return intersectPostingsCardinality(*postings), nil
	}
}

// Filter appends the candidates holding every compiled value to dst. It
// materializes the match once, then probes candidates with a cursor, so it
// tolerates unsorted input.
func (q Query) Filter(dst, candidates []ImageID) ([]ImageID, error) {
	match, err := q.Match()
	if err != nil {
		return dst, err
	}
	return filterBitmap(dst, candidates, match), nil
}
