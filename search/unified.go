package search

import (
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

// ErrIncompatibleEntity reports a query whose entity kind has no liveness
// namespace in the unified Store, or an entity-mismatched clause set.
var ErrIncompatibleEntity = errors.New("search: incompatible entity kind")

// ErrNamespaceUnavailable reports that a clause references a namespace that is
// missing, corrupt, or stale (degraded after a partially failed batch).
var ErrNamespaceUnavailable = errors.New("search: namespace unavailable")

// Engine binds a compiled-query package to a unified index.Store. It resolves
// typed engines by namespace and automatically restricts every compiled query
// to live entities of one kind, so a logically deleted entity can never be
// returned.
type Engine struct {
	db *index.Store
}

// New binds the unified Store to the query planner. The Store must remain open
// for the lifetime of the Engine.
func New(db *index.Store) (*Engine, error) {
	if db == nil {
		return nil, errors.New("search: nil unified store")
	}
	return &Engine{db: db}, nil
}

// Store returns the bound unified Store.
func (e *Engine) Store() *index.Store { return e.db }

// Time returns the timestamp engine for a namespace, or false if unavailable.
func (e *Engine) Time(ns common.Namespace) (*timeidx.Index, bool) { return e.db.Time(ns) }

// Geo returns the geographic engine for a namespace, or false if unavailable.
func (e *Engine) Geo(ns common.Namespace) (*geoidx.Index, bool) { return e.db.Geo(ns) }

// Bitmap returns the bitmap engine for a namespace, or false if unavailable.
func (e *Engine) Bitmap(ns common.Namespace) (*bitmapidx.Index, bool) { return e.db.Bitmap(ns) }

// Hash returns an exact-hash engine for a namespace, or false if unavailable.
func (e *Engine) Hash(ns common.Namespace) (*radixhash.Index, bool) { return e.db.Hash(ns) }

// During builds a timestamp clause for a namespace.
func (e *Engine) During(ns common.Namespace, start, end uint64, options ...TimestampOption) (Clause, error) {
	idx, ok := e.db.Time(ns)
	if !ok {
		return nil, ErrNamespaceUnavailable
	}
	return During(idx, start, end, options...), nil
}

// InCells builds a geographic clause for a namespace.
func (e *Engine) InCells(ns common.Namespace, cellIDs ...uint64) (Clause, error) {
	idx, ok := e.db.Geo(ns)
	if !ok {
		return nil, ErrNamespaceUnavailable
	}
	return InCells(idx, cellIDs...), nil
}

// Values builds a bitmap value clause for a namespace.
func (e *Engine) Values(ns common.Namespace, values ...bitmapidx.Value) (Clause, error) {
	idx, ok := e.db.Bitmap(ns)
	if !ok {
		return nil, ErrNamespaceUnavailable
	}
	return AllValues(idx, values...), nil
}

// BSIRange builds an unsigned range clause for a BSI bitmap namespace.
func (e *Engine) BSIRange(ns common.Namespace, bounds bitmapidx.ValueRange) (Clause, error) {
	idx, ok := e.db.Bitmap(ns)
	if !ok {
		return nil, ErrNamespaceUnavailable
	}
	return NamespaceBSIRange(idx, bounds), nil
}

// ExactHash binds a digest clause to an exact-hash namespace in this Store.
// The algorithm is caller-supplied metadata; radixhash does not persist it.
func (e *Engine) ExactHash(ns common.Namespace, algorithm HashAlgorithm, digest [32]byte) (Clause, error) {
	idx, ok := e.db.Hash(ns)
	if !ok {
		return nil, ErrNamespaceUnavailable
	}
	return hashClause{index: idx, namespace: ns, algorithm: algorithm, hash: radixhash.Hash(digest)}, nil
}

// Compile compiles clauses and prepends the entity liveness restriction.
//
// Every entity-scoped clause must reference a namespace owned by the bound
// Store and carrying the requested EntityKind; a mismatch, an unavailable
// namespace, or an index from another Store is rejected with
// ErrIncompatibleEntity before any plan is built. Because ValueIDs can overlap
// across entity kinds, this prevents logically invalid intersections such as an
// Image namespace intersected with a Face namespace.
//
// WithACL clauses use image-ID grants and are only accepted for EntityImage;
// when present the plan uses the Store's dedicated bitmap ACL namespaces.
// ExactHash clauses must be created through Engine.ExactHash so their owning
// namespace can be validated.
func (e *Engine) Compile(entity common.EntityKind, clauses ...Clause) (Plan, error) {
	live, value, ok := e.db.Liveness(entity)
	if !ok {
		return Plan{}, ErrIncompatibleEntity
	}
	if err := e.validateClauses(entity, clauses); err != nil {
		return Plan{}, err
	}
	all := make([]Clause, 0, len(clauses)+1)
	all = append(all, AllValues(live, value))
	all = append(all, clauses...)
	if hasACL(clauses) {
		return CompileWithStore(e.db.BitmapStore(), all...)
	}
	return Compile(all...)
}

// validateClauses resolves each clause's namespace and enforces that it belongs
// to the requested entity kind and to this Store. It runs once per compilation,
// never per result.
func (e *Engine) validateClauses(entity common.EntityKind, clauses []Clause) error {
	for _, c := range clauses {
		if c == nil {
			continue
		}
		switch cl := c.(type) {
		case namespaceClause:
			if cl.index == nil {
				return ErrInvalidClause
			}
			ns := cl.index.Spec().Namespace
			idx, ok := e.db.Bitmap(ns)
			if !ok {
				return ErrNamespaceUnavailable
			}
			if idx != cl.index {
				return ErrIncompatibleEntity
			}
			if ent, ok := e.db.EntityOf(ns); !ok || ent != entity {
				return ErrIncompatibleEntity
			}
		case timestampClause:
			if cl.index == nil {
				return ErrInvalidClause
			}
			ns := cl.index.Namespace()
			idx, ok := e.db.Time(ns)
			if !ok {
				return ErrNamespaceUnavailable
			}
			if idx != cl.index {
				return ErrIncompatibleEntity
			}
			if ent, ok := e.db.EntityOf(ns); !ok || ent != entity {
				return ErrIncompatibleEntity
			}
		case geoClause:
			if cl.index == nil {
				return ErrInvalidClause
			}
			ns := cl.index.Namespace()
			idx, ok := e.db.Geo(ns)
			if !ok {
				return ErrNamespaceUnavailable
			}
			if idx != cl.index {
				return ErrIncompatibleEntity
			}
			if ent, ok := e.db.EntityOf(ns); !ok || ent != entity {
				return ErrIncompatibleEntity
			}
		case aclClause:
			// ACL postings contain image IDs; other entity kinds can reuse the
			// same numeric IDs and must never inherit image grants.
			if entity != common.EntityImage {
				return ErrIncompatibleEntity
			}
		case hashClause:
			if cl.index == nil || cl.namespace == 0 {
				return ErrInvalidClause
			}
			idx, ok := e.db.Hash(cl.namespace)
			if !ok {
				return ErrNamespaceUnavailable
			}
			if idx != cl.index {
				return ErrIncompatibleEntity
			}
			if ent, ok := e.db.EntityOf(cl.namespace); !ok || ent != entity {
				return ErrIncompatibleEntity
			}
		default:
			return ErrIncompatibleEntity
		}
	}
	return nil
}

func hasACL(clauses []Clause) bool {
	for _, c := range clauses {
		if _, ok := c.(aclClause); ok {
			return true
		}
	}
	return false
}
