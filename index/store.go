package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	storeMagic    uint32 = 0x58444955 // UIDX
	storeVersion  uint16 = 1
	storeBinName         = "store.bin"
	schemaBinName        = "schema.bin"
	storeLockName        = "store.lock"
)

// Store is the unified index front door. It owns every engine instance for one
// root directory. The namespace registry is immutable after Create/Open;
// readers use the engines' lock-free paths, and writers serialize on mu.
type Store struct {
	dir  string
	opts Options

	schema   *common.Schema
	defs     []common.NamespaceDef
	reg      registry
	health   []NamespaceHealth // dense by namespace number
	healthOK bool

	bitmapStore *bitmapidx.Store
	timeStore   *timeidx.Store

	lock *os.File

	mu     sync.Mutex // serializes Apply/lifecycle
	closed atomic.Bool
	source uint64 // guarded by mu
	gen    uint64 // guarded by mu
}

// Create builds a new unified Store at dir, creating every configured
// namespace. It fails if a unified store already exists there.
func Create(dir string, spec Spec, opts ...Option) (*Store, error) {
	defs, err := validateSchema(spec.Schema)
	if err != nil {
		return nil, err
	}
	options := applyOptions(opts)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, storeBinName)); err == nil {
		return nil, fmt.Errorf("%w: %s already exists", ErrInvalidSchema, storeBinName)
	}
	s := &Store{dir: dir, opts: options, schema: spec.Schema, defs: defs}
	// bitmapidx.NewStore requires an empty directory (its exclusive lock checks
	// for foreign files), so every engine is created before the unified
	// manifest and lock exist.
	if err := s.openEngines(true); err != nil {
		return nil, err
	}
	for _, h := range s.Health() {
		if !h.Available {
			_ = s.Close()
			return nil, fmt.Errorf("%w: namespace %d: %w", ErrNamespaceUnavailable, h.Namespace, h.Err)
		}
	}
	lock, err := storage.AcquireLock(dir, storeLockName, false)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.lock = lock
	if err := s.writeSchema(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Open reconstructs a Store from the persisted schema. Individual namespace
// failures are reported through Health and do not prevent healthy namespaces
// from opening; a query that needs an unavailable namespace fails explicitly.
func Open(dir string, opts ...Option) (*Store, error) {
	options := applyOptions(opts)
	schema, source, err := readSchema(dir)
	if err != nil {
		return nil, err
	}
	defs, err := validateSchema(schema)
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, storeLockName, false)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opts: options, schema: schema, defs: defs, lock: lock, source: source}
	if err := s.openEngines(false); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return s, nil
}

func applyOptions(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o.withDefaults()
}

func (s *Store) writeSchema() error {
	schemaBytes, err := s.schema.MarshalBinary()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dir, schemaBinName, schemaBytes); err != nil {
		return err
	}
	return s.writeManifest()
}

// writeManifest persists the unified manifest, including the tracked canonical
// database watermark. It is called on Create and whenever the watermark
// advances.
func (s *Store) writeManifest() error {
	var manifest [20]byte
	binary.LittleEndian.PutUint32(manifest[0:4], storeMagic)
	binary.LittleEndian.PutUint16(manifest[4:6], storeVersion)
	binary.LittleEndian.PutUint64(manifest[8:16], s.source)
	binary.LittleEndian.PutUint32(manifest[16:20], storage.Checksum(manifest[:16]))
	return writeFileAtomic(s.dir, storeBinName, manifest[:])
}

func readSchema(dir string) (*common.Schema, uint64, error) {
	manifest, err := os.ReadFile(filepath.Join(dir, storeBinName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("%w: %s missing", ErrInvalidSchema, storeBinName)
		}
		return nil, 0, err
	}
	if len(manifest) < 20 || binary.LittleEndian.Uint32(manifest[0:4]) != storeMagic {
		return nil, 0, ErrCorrupt
	}
	if v := binary.LittleEndian.Uint16(manifest[4:6]); v != storeVersion {
		return nil, 0, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, v)
	}
	if storage.Checksum(manifest[:16]) != binary.LittleEndian.Uint32(manifest[16:20]) {
		return nil, 0, ErrCorrupt
	}
	source := binary.LittleEndian.Uint64(manifest[8:16])
	schemaBytes, err := os.ReadFile(filepath.Join(dir, schemaBinName))
	if err != nil {
		return nil, 0, err
	}
	schema := common.NewSchema()
	if err := schema.UnmarshalBinary(schemaBytes); err != nil {
		return nil, 0, errors.Join(ErrCorrupt, err)
	}
	return schema, source, nil
}

func writeFileAtomic(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return storage.SyncDir(dir)
}

// ---- engine construction ----

func (s *Store) openEngines(create bool) error {
	var maxNS common.Namespace
	for _, def := range s.defs {
		if def.Namespace > maxNS {
			maxNS = def.Namespace
		}
	}
	if int(maxNS) == int(livenessNamespace) {
		maxNS = livenessNamespace - 1
	}
	s.reg.routes = make([]route, int(maxNS)+1)
	for i := range s.reg.routes {
		s.reg.routes[i].slot = noSlot
	}
	s.health = make([]NamespaceHealth, int(maxNS)+1)
	for i := range s.health {
		s.health[i].Namespace = common.Namespace(i)
	}
	for e := common.EntityImage; e.Valid(); e++ {
		s.reg.liveVal[e] = livenessValues[e]
	}

	if err := s.openBitmap(create); err != nil {
		s.markMatching(func(def common.NamespaceDef) bool { return def.Kind.IsBitmap() }, err)
	} else {
		s.routeBitmaps()
	}
	if err := s.openTime(create); err != nil {
		s.markMatching(func(def common.NamespaceDef) bool { return def.Kind == common.KindTimestamp }, err)
	}
	s.openGeos(create)
	s.openSims(create)
	s.openRadixes(create)
	s.openVectors(create)
	s.healthOK = true
	return nil
}

func (s *Store) routeBitmaps() {
	for _, def := range s.defs {
		if !def.Kind.IsBitmap() {
			continue
		}
		h := &s.health[def.Namespace]
		h.Kind, h.Entity = def.Kind, def.Entity
		if s.bitmapStore == nil {
			continue
		}
		idx, ok := s.bitmapStore.Lookup(def.Namespace)
		if !ok {
			h.Err = ErrNamespaceUnavailable
			continue
		}
		s.reg.bitmaps = append(s.reg.bitmaps, idx)
		s.setRoute(def, uint16(len(s.reg.bitmaps)-1))
	}
	if s.bitmapStore != nil {
		if live, ok := s.bitmapStore.Lookup(livenessNamespace); ok {
			s.reg.live = live
		}
	}
}

func (s *Store) openBitmap(create bool) error {
	var specs []bitmapidx.NamespaceSpec
	for _, def := range s.defs {
		if !def.Kind.IsBitmap() {
			continue
		}
		spec, err := bitmapidx.NamespaceSpecFromSchemaDef(def)
		if err != nil {
			// Validate the definition now and record a per-namespace failure.
			h := &s.health[def.Namespace]
			h.Kind, h.Entity = def.Kind, def.Entity
			h.Err = errors.Join(ErrInvalidSchema, err)
			h.Corrupt = true
			continue
		}
		specs = append(specs, spec)
	}
	specs = append(specs, bitmapidx.NamespaceSpec{
		Namespace: livenessNamespace,
		Kind:      common.KindBitmapEnum,
		Name:      livenessName,
	})
	var err error
	if create {
		s.bitmapStore, err = bitmapidx.NewStore(s.dir, specs, bitmapidx.NamespaceConfig{}, s.opts.Durability)
	} else {
		s.bitmapStore, err = bitmapidx.OpenStore(s.dir, bitmapidx.NamespaceConfig{}, s.opts.Durability)
	}
	return err
}

func (s *Store) openTime(create bool) error {
	var inits []timeidx.NamespaceInit
	for _, def := range s.defs {
		if def.Kind != common.KindTimestamp {
			continue
		}
		inits = append(inits, timeidx.NamespaceInit{
			Spec:   timeidx.Spec{Namespace: def.Namespace, Name: def.Name},
			Create: create,
		})
	}
	if len(inits) == 0 {
		return nil
	}
	store, results, err := timeidx.InitializeStore(s.dir, inits, timeidx.Config{}, s.opts.Durability)
	if err != nil {
		return err
	}
	s.timeStore = store
	for _, res := range results {
		def, _ := s.schema.Lookup(res.Namespace)
		h := &s.health[res.Namespace]
		h.Kind, h.Entity = def.Kind, def.Entity
		if res.Err != nil {
			h.Err = res.Err
			h.NeedsRebuild = needsRebuild(res.Err)
			h.Corrupt = isCorrupt(res.Err)
			continue
		}
		idx, ok := store.Namespace(res.Namespace)
		if !ok {
			h.Err = ErrNamespaceUnavailable
			continue
		}
		s.reg.times = append(s.reg.times, idx)
		s.setRoute(def, uint16(len(s.reg.times)-1))
	}
	return nil
}

func (s *Store) openGeos(create bool) {
	for _, def := range s.defs {
		if def.Kind != common.KindGeo {
			continue
		}
		path := filepath.Join(s.dir, fmt.Sprintf("geo-%04x.idx", def.Namespace))
		cfg := geoidx.Config{Namespace: def.Namespace}
		var (
			idx *geoidx.Index
			err error
		)
		if create {
			idx, err = geoidx.NewFile(path, cfg)
		} else {
			idx, err = geoidx.OpenFile(path, cfg)
		}
		s.attach(def, err, func() { s.reg.geos = append(s.reg.geos, idx); s.setRoute(def, uint16(len(s.reg.geos)-1)) })
	}
}

func (s *Store) openSims(create bool) {
	for _, def := range s.defs {
		if def.Kind != common.KindHashPerceptual {
			continue
		}
		prefix := filepath.Join(s.dir, fmt.Sprintf("sim-%04x", def.Namespace))
		if !create {
			if _, err := os.Stat(prefix + ".manifest"); err != nil {
				s.markUnavailable(def, fmt.Errorf("%w: missing manifest", ErrNamespaceUnavailable))
				continue
			}
		}
		st, err := simhash.OpenFile(prefix, simhash.Config{Durability: s.opts.Durability})
		s.attach(def, err, func() { s.reg.sims = append(s.reg.sims, st); s.setRoute(def, uint16(len(s.reg.sims)-1)) })
	}
}

func (s *Store) openRadixes(create bool) {
	for _, def := range s.defs {
		if def.Kind != common.KindHashExact {
			continue
		}
		path := filepath.Join(s.dir, fmt.Sprintf("radix-%04x.idx", def.Namespace))
		var (
			idx *radixhash.Index
			err error
		)
		if create {
			idx, err = radixhash.NewMmap(path, s.opts.RadixArenaBytes, radixhash.Config{})
		} else {
			idx, err = radixhash.OpenMmap(path, radixhash.Config{})
		}
		s.attach(def, err, func() { s.reg.radixs = append(s.reg.radixs, idx); s.setRoute(def, uint16(len(s.reg.radixs)-1)) })
	}
}

func (s *Store) openVectors(create bool) {
	for _, def := range s.defs {
		if def.Kind != common.KindVector {
			continue
		}
		path := filepath.Join(s.dir, fmt.Sprintf("vector-%04x.idx", def.Namespace))
		want, err := decodeVectorConfig(def.Config)
		if err != nil {
			s.markUnavailable(def, err)
			continue
		}
		want.Namespace = def.Namespace
		var (
			idx *vectoridx.VectorIdx
		)
		if create {
			idx, err = vectoridx.NewStore(path, want, s.opts.Durability)
		} else {
			idx, err = vectoridx.OpenStore(path, s.opts.Durability)
		}
		if err != nil {
			s.markUnavailable(def, err)
			continue
		}
		got := idx.Config()
		if got.Dimensions != want.Dimensions || got.ElementType != want.ElementType || got.Metric != want.Metric {
			_ = idx.Close()
			s.markUnavailable(def, fmt.Errorf("%w: vector config mismatch", ErrInvalidSchema))
			continue
		}
		s.reg.vectors = append(s.reg.vectors, idx)
		s.setRoute(def, uint16(len(s.reg.vectors)-1))
	}
}

// attach records health and, when idx opened, appends it to the engine slice
// and installs the route.
func (s *Store) attach(def common.NamespaceDef, err error, ok func()) {
	h := &s.health[def.Namespace]
	h.Kind, h.Entity = def.Kind, def.Entity
	if err != nil {
		h.Err = err
		h.NeedsRebuild = needsRebuild(err)
		h.Corrupt = isCorrupt(err)
		return
	}
	ok()
}

func (s *Store) setRoute(def common.NamespaceDef, slot uint16) {
	s.reg.routes[def.Namespace] = route{kind: def.Kind, entity: def.Entity, slot: slot, available: true}
	h := &s.health[def.Namespace]
	h.Kind, h.Entity, h.Available = def.Kind, def.Entity, true
}

// markMatching records an engine-level failure against every matching
// namespace while leaving unrelated namespaces' health untouched.
func (s *Store) markMatching(match func(common.NamespaceDef) bool, err error) {
	for _, def := range s.defs {
		if match(def) {
			s.markUnavailable(def, err)
		}
	}
}

func (s *Store) markUnavailable(def common.NamespaceDef, err error) {
	h := &s.health[def.Namespace]
	h.Kind, h.Entity = def.Kind, def.Entity
	h.Err = err
	h.NeedsRebuild = needsRebuild(err)
	h.Corrupt = isCorrupt(err)
}

func needsRebuild(err error) bool {
	return errors.Is(err, timeidx.ErrRebuildRequired) || errors.Is(err, common.ErrCorruptSchema) || isCorrupt(err)
}

func isCorrupt(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, timeidx.ErrCorrupt) ||
		errors.Is(err, geoidx.ErrCorrupt) || errors.Is(err, geoidx.ErrLegacyFormat) ||
		errors.Is(err, bitmapidx.ErrCorruptSegment) || errors.Is(err, bitmapidx.ErrCorruptManifest) ||
		errors.Is(err, bitmapidx.ErrCorruptWAL) ||
		errors.Is(err, simhash.ErrCorrupt) || errors.Is(err, radixhash.ErrCorrupt) ||
		errors.Is(err, vectoridx.ErrCorruptStorage)
}

// ---- lifecycle ----

// Dir returns the root directory.
func (s *Store) Dir() string { return s.dir }

// Schema returns the persisted schema (read-only).
func (s *Store) Schema() *common.Schema { return s.schema }

// NamespaceDef returns the persisted definition for a namespace.
func (s *Store) NamespaceDef(ns common.Namespace) (common.NamespaceDef, bool) {
	return s.schema.Lookup(ns)
}

// Health returns per-namespace open state for every declared namespace.
func (s *Store) Health() []NamespaceHealth {
	if !s.healthOK {
		return nil
	}
	out := make([]NamespaceHealth, 0, len(s.defs))
	for _, def := range s.defs {
		out = append(out, s.health[def.Namespace])
	}
	return out
}

// HealthOf returns the open state of one namespace.
func (s *Store) HealthOf(ns common.Namespace) (NamespaceHealth, bool) {
	if !s.healthOK || int(ns) >= len(s.health) {
		return NamespaceHealth{}, false
	}
	return s.health[ns], true
}

// SourceGeneration returns the tracked canonical-database completeness
// watermark. Zero means unrecorded.
func (s *Store) SourceGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source
}

// Generation returns the Store's own monotonic batch generation.
func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Close drains and closes every owned engine. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	var err error
	if s.bitmapStore != nil {
		err = errors.Join(err, s.bitmapStore.Close())
	}
	if s.timeStore != nil {
		err = errors.Join(err, s.timeStore.Close())
	}
	for _, g := range s.reg.geos {
		err = errors.Join(err, g.Close())
	}
	for _, st := range s.reg.sims {
		err = errors.Join(err, st.Close())
	}
	for _, r := range s.reg.radixs {
		err = errors.Join(err, r.Close())
	}
	for _, v := range s.reg.vectors {
		err = errors.Join(err, v.Close())
	}
	return errors.Join(err, storage.ReleaseLock(s.lock))
}

// Sync flushes every engine that exposes a synchronization barrier.
func (s *Store) Sync() error {
	if s.closed.Load() {
		return ErrClosed
	}
	var err error
	if s.bitmapStore != nil {
		err = errors.Join(err, s.bitmapStore.Checkpoint())
	}
	err = errors.Join(err, s.eachTime(func(i *timeidx.Index) error { return i.Sync() }))
	for _, g := range s.reg.geos {
		err = errors.Join(err, g.Sync())
	}
	for _, st := range s.reg.sims {
		err = errors.Join(err, st.Sync())
	}
	for _, r := range s.reg.radixs {
		err = errors.Join(err, r.Sync())
	}
	for _, v := range s.reg.vectors {
		err = errors.Join(err, v.Sync())
	}
	return err
}

// Checkpoint flushes every engine's durable checkpoint.
func (s *Store) Checkpoint() error {
	if s.closed.Load() {
		return ErrClosed
	}
	var err error
	if s.bitmapStore != nil {
		err = errors.Join(err, s.bitmapStore.Checkpoint())
	}
	err = errors.Join(err, s.eachTime(func(i *timeidx.Index) error { return i.Checkpoint() }))
	for _, g := range s.reg.geos {
		err = errors.Join(err, g.Checkpoint())
	}
	for _, st := range s.reg.sims {
		err = errors.Join(err, st.Checkpoint())
	}
	for _, r := range s.reg.radixs {
		err = errors.Join(err, r.Checkpoint())
	}
	for _, v := range s.reg.vectors {
		err = errors.Join(err, v.Checkpoint())
	}
	return err
}

// Compact compacts every engine that supports it.
func (s *Store) Compact() error {
	if s.closed.Load() {
		return ErrClosed
	}
	var err error
	for _, b := range s.reg.bitmaps {
		err = errors.Join(err, b.Compact())
	}
	for _, g := range s.reg.geos {
		err = errors.Join(err, g.Compact())
	}
	for _, r := range s.reg.radixs {
		err = errors.Join(err, r.Compact())
	}
	for _, v := range s.reg.vectors {
		if _, cerr := v.Compact(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}
	return err
}

// Verify validates every namespace file without mutating it. It validates what
// each engine exposes; engines without a standalone verifier rely on their Open
// validation.
func (s *Store) Verify() error {
	var err error
	for _, def := range s.defs {
		switch familyOf(def.Kind) {
		case familyTime:
			if verr := timeidx.VerifyNamespace(s.dir, def.Namespace); verr != nil {
				err = errors.Join(err, verr)
			}
		case familyVector:
			path := filepath.Join(s.dir, fmt.Sprintf("vector-%04x.idx", def.Namespace))
			if verr := vectoridx.VerifyFile(path); verr != nil {
				err = errors.Join(err, verr)
			}
		default:
			// Engines without a standalone verifier validate during Open; a
			// missing file is the only cheap check here.
			if !s.health[def.Namespace].Available {
				err = errors.Join(err, fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, def.Namespace))
			}
		}
	}
	return err
}

func (s *Store) eachTime(fn func(*timeidx.Index) error) error {
	var err error
	for _, idx := range s.reg.times {
		err = errors.Join(err, fn(idx))
	}
	return err
}

// ---- typed accessors for the search planner ----

// Time returns a timestamp namespace index if it is available.
func (s *Store) Time(ns common.Namespace) (*timeidx.Index, bool) { return s.reg.timeIndex(ns) }

// Geo returns a geographic namespace index if it is available.
func (s *Store) Geo(ns common.Namespace) (*geoidx.Index, bool) { return s.reg.geoIndex(ns) }

// Sim returns a perceptual-similarity namespace store if it is available.
func (s *Store) Sim(ns common.Namespace) (*simhash.Store, bool) { return s.reg.simStore(ns) }

// Hash returns an exact-hash namespace index if it is available.
func (s *Store) Hash(ns common.Namespace) (*radixhash.Index, bool) { return s.reg.radixIndex(ns) }

// Vector returns a vector namespace index if it is available.
func (s *Store) Vector(ns common.Namespace) (*vectoridx.VectorIdx, bool) {
	return s.reg.vectorIndex(ns)
}

// Bitmap returns a bitmap namespace index if it is available.
func (s *Store) Bitmap(ns common.Namespace) (*bitmapidx.Index, bool) {
	return s.reg.bitmapIndex(ns)
}

// Liveness returns the entity liveness bitmap and its enum value.
func (s *Store) Liveness(entity common.EntityKind) (*bitmapidx.Index, bitmapidx.Value, bool) {
	return s.reg.liveness(entity)
}

// EntityOf returns the entity kind of a namespace.
func (s *Store) EntityOf(ns common.Namespace) (common.EntityKind, bool) {
	if int(ns) >= len(s.reg.routes) {
		return common.EntityInvalid, false
	}
	rt := s.reg.routes[ns]
	if rt.slot == noSlot {
		return common.EntityInvalid, false
	}
	return rt.entity, true
}
