package index

import (
	"errors"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/roar"
)

// The import matcher is deliberately separate from the general Search planner.
// RadixHash (exact content) and SimHash (perceptual) are used during image
// import to find exact duplicates and visually similar images; they are not
// search clauses.

// ErrMatcherNamespace reports a matcher namespace that is missing, not an
// EntityImage namespace, or not the expected exact/perceptual kind.
var ErrMatcherNamespace = errors.New("index: matcher namespace must be an EntityImage exact or perceptual namespace")

// MatchKind identifies which matcher produced a hit.
type MatchKind uint8

const (
	MatchSHA256 MatchKind = iota + 1
	MatchBLAKE3
	MatchPHash
	MatchPDQ
)

// MatcherSpec binds the import matcher to specific namespaces. Zero namespaces
// are unbound and skipped. SHA256 and BLAKE3 must be KindHashExact namespaces;
// Perceptual must be a KindHashPerceptual namespace. All must carry
// EntityImage.
type MatcherSpec struct {
	SHA256     common.Namespace
	BLAKE3     common.Namespace
	Perceptual common.Namespace
}

// Matcher resolves import-matching namespaces once. It is safe for concurrent
// use by any number of readers (each call uses only the bound engines' lock-free
// paths and caller-owned buffers).
type Matcher struct {
	store      *Store
	sha256     *radixhash.Index
	blake3     *radixhash.Index
	perceptual *simhash.Store
	live       *bitmapidx.Index
	liveVal    bitmapidx.Value

	// resPool reuses perceptual-result buffers across queries; safe for
	// concurrent readers. Exact lookups use a small stack buffer instead.
	resPool sync.Pool // []simhash.Result

	// liveCache memoizes the live-entity bitmap per liveness version so
	// steady-state matching allocates nothing while never serving a cached
	// bitmap older than the last committed liveness change.
	liveCache atomic.Pointer[liveCache]
}

type liveCache struct {
	version uint64
	bm      roar.Bitmap
}

// Matcher binds the import matcher to the namespaces in spec.
func (s *Store) Matcher(spec MatcherSpec) (*Matcher, error) {
	live, liveVal, ok := s.Liveness(common.EntityImage)
	if !ok {
		return nil, ErrMatcherNamespace
	}
	m := &Matcher{store: s, live: live, liveVal: liveVal}
	exact := func(ns common.Namespace) (*radixhash.Index, error) {
		if ns == 0 {
			return nil, nil
		}
		if def, ok := s.NamespaceDef(ns); !ok || def.Kind != common.KindHashExact || def.Entity != common.EntityImage {
			return nil, ErrMatcherNamespace
		}
		idx, ok := s.reg.radixIndex(ns)
		if !ok {
			return nil, ErrMatcherNamespace
		}
		return idx, nil
	}
	var err error
	if m.sha256, err = exact(spec.SHA256); err != nil {
		return nil, err
	}
	if m.blake3, err = exact(spec.BLAKE3); err != nil {
		return nil, err
	}
	if spec.Perceptual != 0 {
		if def, ok := s.NamespaceDef(spec.Perceptual); !ok || def.Kind != common.KindHashPerceptual || def.Entity != common.EntityImage {
			return nil, ErrMatcherNamespace
		}
		if m.perceptual, ok = s.reg.simStore(spec.Perceptual); !ok {
			return nil, ErrMatcherNamespace
		}
	}
	return m, nil
}

// SHA256 returns the bound SHA-256 exact engine (nil if unbound).
func (m *Matcher) SHA256() *radixhash.Index { return m.sha256 }

// BLAKE3 returns the bound BLAKE3 exact engine (nil if unbound).
func (m *Matcher) BLAKE3() *radixhash.Index { return m.blake3 }

// Perceptual returns the bound perceptual engine (nil if unbound).
func (m *Matcher) Perceptual() *simhash.Store { return m.perceptual }

// MatchItem is one incoming image's match inputs. Nil fields are skipped.
type MatchItem struct {
	SHA256 *radixhash.Hash
	BLAKE3 *radixhash.Hash
	PHash  *uint64
	PDQ    *simhash.PDQHash
}

// MatchParams configures a batch.
type MatchParams struct {
	// PHashDistance and PDQDistance bound perceptual matches (native Hamming
	// distance, no normalization). PDQDistance is clamped to
	// simhash.MaxPDQSearchDistance.
	PHashDistance uint8
	PDQDistance   uint8
	// DetectIntraBatch also reports duplicates among the incoming items.
	DetectIntraBatch bool
}

// Match is one import-matching hit. ImageID is the matched, live image when the
// hit came from the index; Peer is the index of the other incoming item when
// Intra is true. Distance is the native Hamming distance (0 for exact hits).
type Match struct {
	Item     int
	Kind     MatchKind
	ImageID  common.ValueID
	Distance uint16
	Intra    bool
	Peer     int
}

// Match appends import matches for every item to dst, restricted to live
// EntityImage entities. Results are deterministic: for each item, index hits
// follow the engine's ascending-ID order, then (when requested) intra-batch
// peers follow in ascending peer order. The caller owns dst; with sufficient
// capacity and a bounded live set only the live bitmap is allocated.
func (m *Matcher) Match(dst []Match, items []MatchItem, params MatchParams) ([]Match, error) {
	live, err := m.liveBitmap()
	if err != nil {
		return dst, err
	}
	for i := range items {
		it := &items[i]
		if it.SHA256 != nil && m.sha256 != nil {
			dst = m.exactInto(dst, i, MatchSHA256, m.sha256, *it.SHA256, live)
		}
		if it.BLAKE3 != nil && m.blake3 != nil {
			dst = m.exactInto(dst, i, MatchBLAKE3, m.blake3, *it.BLAKE3, live)
		}
		if it.PHash != nil && m.perceptual != nil {
			dst = m.pHashInto(dst, i, *it.PHash, params.PHashDistance, live)
		}
		if it.PDQ != nil && m.perceptual != nil {
			dst = m.pdqInto(dst, i, it.PDQ, params.PDQDistance, live)
		}
	}
	if params.DetectIntraBatch {
		dst = m.intraBatch(dst, items, params)
	}
	return dst, nil
}

// MatchOne matches a single incoming image. It avoids allocating a one-element
// slice.
func (m *Matcher) MatchOne(dst []Match, item MatchItem, params MatchParams) ([]Match, error) {
	items := [1]MatchItem{item}
	return m.Match(dst, items[:], params)
}

func (m *Matcher) liveBitmap() (roar.Bitmap, error) {
	version := m.store.liveVersion.Load()
	if c := m.liveCache.Load(); c != nil && c.version == version {
		return c.bm, nil
	}
	bm, err := m.live.MatchValues(m.liveVal)
	if err != nil {
		return roar.Bitmap{}, err
	}
	m.liveCache.Store(&liveCache{version: version, bm: bm})
	return bm, nil
}

// exactInto appends the live index IDs whose exact hash equals h. Exact hashes
// usually have few duplicates, so a small stack buffer avoids per-call
// allocation; a duplicate-heavy hash grows it as needed.
func (m *Matcher) exactInto(dst []Match, item int, kind MatchKind, idx *radixhash.Index, h radixhash.Hash, live roar.Bitmap) []Match {
	var stack [16]radixhash.ImageID
	ids := idx.Lookup(stack[:0], h)
	for _, id := range ids {
		if live.Contains(uint64(id)) {
			dst = append(dst, Match{Item: item, Kind: kind, ImageID: id})
		}
	}
	return dst
}

// pHashInto appends live matches within maxDistance of query.
func (m *Matcher) pHashInto(dst []Match, item int, query uint64, maxDistance uint8, live roar.Bitmap) []Match {
	scratch := m.getResults()
	results := m.perceptual.SearchPHash(scratch, query, maxDistance)
	for _, r := range results {
		if live.Contains(uint64(r.ImageID)) {
			dst = append(dst, Match{Item: item, Kind: MatchPHash, ImageID: r.ImageID, Distance: uint16(r.Distance)})
		}
	}
	m.putResults(results[:0])
	return dst
}

// pdqInto appends live matches within maxDistance of query.
func (m *Matcher) pdqInto(dst []Match, item int, query *simhash.PDQHash, maxDistance uint8, live roar.Bitmap) []Match {
	if maxDistance > simhash.MaxPDQSearchDistance {
		maxDistance = simhash.MaxPDQSearchDistance
	}
	scratch := m.getResults()
	results := m.perceptual.SearchPDQ(scratch, query, maxDistance)
	for _, r := range results {
		if live.Contains(uint64(r.ImageID)) {
			dst = append(dst, Match{Item: item, Kind: MatchPDQ, ImageID: r.ImageID, Distance: uint16(r.Distance)})
		}
	}
	m.putResults(results[:0])
	return dst
}

func (m *Matcher) getResults() []simhash.Result {
	if v, ok := m.resPool.Get().([]simhash.Result); ok {
		return v[:0]
	}
	return nil
}

func (m *Matcher) putResults(results []simhash.Result) {
	if cap(results) != 0 {
		m.resPool.Put(results[:0])
	}
}

// intraBatch reports duplicates among the incoming items themselves. It is
// O(n^2) for small import batches and allocates nothing.
func (m *Matcher) intraBatch(dst []Match, items []MatchItem, params MatchParams) []Match {
	for i := 0; i < len(items); i++ {
		a := &items[i]
		for j := i + 1; j < len(items); j++ {
			b := &items[j]
			if a.SHA256 != nil && b.SHA256 != nil && *a.SHA256 == *b.SHA256 {
				dst = append(dst,
					Match{Item: i, Kind: MatchSHA256, Intra: true, Peer: j},
					Match{Item: j, Kind: MatchSHA256, Intra: true, Peer: i})
			}
			if a.BLAKE3 != nil && b.BLAKE3 != nil && *a.BLAKE3 == *b.BLAKE3 {
				dst = append(dst,
					Match{Item: i, Kind: MatchBLAKE3, Intra: true, Peer: j},
					Match{Item: j, Kind: MatchBLAKE3, Intra: true, Peer: i})
			}
			if a.PHash != nil && b.PHash != nil {
				if d := simhash.DistancePHash(*a.PHash, *b.PHash); d <= params.PHashDistance {
					dst = append(dst,
						Match{Item: i, Kind: MatchPHash, Distance: uint16(d), Intra: true, Peer: j},
						Match{Item: j, Kind: MatchPHash, Distance: uint16(d), Intra: true, Peer: i})
				}
			}
			if a.PDQ != nil && b.PDQ != nil {
				maxD := params.PDQDistance
				if maxD > simhash.MaxPDQSearchDistance {
					maxD = simhash.MaxPDQSearchDistance
				}
				if d := simhash.DistancePDQ(a.PDQ, b.PDQ); d <= uint16(maxD) {
					dst = append(dst,
						Match{Item: i, Kind: MatchPDQ, Distance: d, Intra: true, Peer: j},
						Match{Item: j, Kind: MatchPDQ, Distance: d, Intra: true, Peer: i})
				}
			}
		}
	}
	return dst
}
