package search

import (
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

// ErrIncompatibleEntity reports a query whose entity kind has no liveness
// namespace in the unified Store, or an entity-mismatched clause set.
var ErrIncompatibleEntity = errors.New("search: incompatible entity kind")

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

// During builds a timestamp clause for a namespace.
func (e *Engine) During(ns common.Namespace, start, end uint64, options ...TimestampOption) (Clause, error) {
	idx, ok := e.db.Time(ns)
	if !ok {
		return nil, ErrIncompatibleEntity
	}
	return During(idx, start, end, options...), nil
}

// InCells builds a geographic clause for a namespace.
func (e *Engine) InCells(ns common.Namespace, cellIDs ...uint64) (Clause, error) {
	idx, ok := e.db.Geo(ns)
	if !ok {
		return nil, ErrIncompatibleEntity
	}
	return InCells(idx, cellIDs...), nil
}

// Values builds a bitmap value clause for a namespace.
func (e *Engine) Values(ns common.Namespace, values ...bitmapidx.Value) (Clause, error) {
	idx, ok := e.db.Bitmap(ns)
	if !ok {
		return nil, ErrIncompatibleEntity
	}
	return AllValues(idx, values...), nil
}

// Compile compiles clauses and prepends the entity liveness restriction.
// Clauses on unavailable namespaces fail through their constructors, so a
// query that depends on a broken namespace cannot silently return incomplete
// results.
func (e *Engine) Compile(entity common.EntityKind, clauses ...Clause) (Plan, error) {
	live, value, ok := e.db.Liveness(entity)
	if !ok {
		return Plan{}, ErrIncompatibleEntity
	}
	all := make([]Clause, 0, len(clauses)+1)
	all = append(all, AllValues(live, value))
	all = append(all, clauses...)
	return Compile(all...)
}
