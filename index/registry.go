package index

import (
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// noSlot marks a namespace with no resolved engine route.
const noSlot uint16 = ^uint16(0)

// route is the immutable, allocation-free routing entry for one namespace. It
// is resolved once during Create/Open; search and mutation never do string or
// map lookups.
type route struct {
	kind      schema.NamespaceKind
	entity    schema.EntityKind
	slot      uint16
	available bool
}

// registry is the immutable published namespace → engine table. It is replaced
// wholesale during Create/Open and never mutated afterwards.
type registry struct {
	routes  []route // indexed directly by schema.Namespace
	times   []*timeidx.Index
	geos    []*geoidx.Index
	sims    []*simhash.Store
	radixs  []*radixhash.Index
	vectors []*vectoridx.VectorIdx
	bitmaps []*bitmapidx.Index

	live    *bitmapidx.Index
	liveVal [schema.EntityKindCount + 1]bitmapidx.Value
}

func (r *registry) lookup(ns schema.Namespace) (route, bool) {
	if int(ns) >= len(r.routes) {
		return route{}, false
	}
	rt := r.routes[ns]
	if rt.slot == noSlot {
		return route{}, false
	}
	return rt, true
}

func (r *registry) timeIndex(ns schema.Namespace) (*timeidx.Index, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.times) {
		return nil, false
	}
	return r.times[rt.slot], true
}

func (r *registry) geoIndex(ns schema.Namespace) (*geoidx.Index, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.geos) {
		return nil, false
	}
	return r.geos[rt.slot], true
}

func (r *registry) simStore(ns schema.Namespace) (*simhash.Store, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.sims) {
		return nil, false
	}
	return r.sims[rt.slot], true
}

func (r *registry) radixIndex(ns schema.Namespace) (*radixhash.Index, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.radixs) {
		return nil, false
	}
	return r.radixs[rt.slot], true
}

func (r *registry) vectorIndex(ns schema.Namespace) (*vectoridx.VectorIdx, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.vectors) {
		return nil, false
	}
	return r.vectors[rt.slot], true
}

func (r *registry) bitmapIndex(ns schema.Namespace) (*bitmapidx.Index, bool) {
	rt, ok := r.lookup(ns)
	if !ok || !rt.available || int(rt.slot) >= len(r.bitmaps) {
		return nil, false
	}
	return r.bitmaps[rt.slot], true
}

// liveness returns the shared liveness bitmap index and the enum value that
// marks an entity of the given kind as live.
func (r *registry) liveness(entity schema.EntityKind) (*bitmapidx.Index, bitmapidx.Value, bool) {
	if !entity.Valid() || r.live == nil {
		return nil, bitmapidx.Value{}, false
	}
	return r.live, r.liveVal[entity], true
}
