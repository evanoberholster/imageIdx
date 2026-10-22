package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/internal/storage"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

const (
	storeMagic    uint32 = 0x58444955 // UIDX
	storeVersion  uint16 = 2          // v2: per-namespace completeness generations
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

	schema     *schematypes.Definitions
	defs       []schematypes.NamespaceDef
	standalone bool // legacy/standalone indexes retain schema.bin for independent Open
	reg        registry
	health     []NamespaceHealth // dense by namespace number
	healthOK   bool

	bitmapStore *bitmapidx.Store
	timeStore   *timeidx.Store

	lock *os.File

	stats indexStats

	mu     sync.Mutex // serializes Apply/lifecycle
	closed atomic.Bool
	source uint64 // last requested watermark; guarded by mu
	gen    uint64 // guarded by mu

	// nsSource holds the confirmed completeness generation per namespace
	// (dense, indexed by namespace number) and safeSource is the store-wide
	// watermark: the minimum across every declared namespace. Readers consult
	// safeSource lock-free; both are guarded by mu on the writer side.
	nsSource   []uint64
	safeSource atomic.Uint64

	// pendingSet is the durable recovery marker: namespaces whose data may be
	// incomplete because an interrupted multi-engine mutation could have made
	// some engines durable and others not. It is written before the mutation
	// can persist and cleared only after the touched engines are durable.
	// Writer-owned (guarded by mu); the persisted form is recovery.bin.
	pendingSet []uint64

	// liveSeq is a seqlock over entity-liveness publication: it is incremented
	// to an odd value before a liveness mutation begins and to an even value
	// after it is published. The import matcher reads it to pair a bitmap
	// snapshot with the exact generation it reflects.
	liveSeq atomic.Uint64

	// liveRemovals records whether any liveness removal has ever been committed.
	// Until one has, every engine record is live and the import matcher can skip
	// the liveness filter entirely. It is persisted in the manifest.
	liveRemovals atomic.Bool

	// degraded is an immutable bitset of namespaces known to be potentially
	// incomplete after a partially failed batch. Readers consult it lock-free;
	// the writer republishes a copy (a rare failure path) while holding mu.
	degraded atomic.Pointer[[]uint64]

	// Writer-owned scratch for batch prevalidation and staged, grouped
	// application. All are only touched under mu and reused across batches.
	pvSet       []uint64
	pvCursors   []simCursor
	pvVectorOps int
	pvSimUpdate bool
	stage       stageState
	flush       flushScratch

	// flushSourceGen / flushExplicit let the timeidx flush commit mutations and
	// the completeness watermark in one engine transaction instead of a second
	// empty-generation commit. Writer-owned.
	flushSourceGen uint64
	flushExplicit  bool

	// crash is a per-Store test seam: when non-nil it is invoked at each
	// staged-flush boundary. It is nil in production and never shared.
	crash func(string)
}

func (s *Store) injectCrash(stage string) {
	if s.crash != nil {
		s.crash(stage)
	}
}

// flushScratch holds reusable per-engine buffers for staged flushing. It is
// writer-owned (no mutex) because writes are serialized.
type flushScratch struct {
	timeBatch *timeidx.Batch
	timeCap   int
	geoIns    []geoidx.Entry
	geoDel    []geoidx.Entry
	simApp    []simhash.Record
	simUpd    []simhash.Update
	hashAdd   []radixhash.Entry
	hashDel   []radixhash.Entry
}

// simCursor tracks the next dense simhash ValueID expected for one namespace
// during prevalidation, so a batch of appends is validated without mutating an
// engine.
type simCursor struct {
	ns   schematypes.Namespace
	next uint64
}

// Create builds a new unified Store at dir, creating every configured
// namespace. It fails if a unified store already exists there.
func Create(dir string, spec Spec, opts ...Option) (*Store, error) {
	return create(dir, spec.Schema, true, opts...)
}

// CreateWithDefinitions creates a database-managed index using the schema's
// authoritative definitions. It writes only physical index metadata, not a
// second logical schema file. Standalone callers may continue using Create.
func CreateWithDefinitions(dir string, definitions *schematypes.Definitions, opts ...Option) (*Store, error) {
	indexes, err := indexDefinitions(definitions)
	if err != nil {
		return nil, err
	}
	return create(dir, indexes, false, opts...)
}

func create(dir string, definitions *schematypes.Definitions, standalone bool, opts ...Option) (*Store, error) {
	defs, err := validateSchema(definitions)
	if err != nil {
		return nil, err
	}
	storedSchema, err := cloneSchema(definitions)
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
	s := &Store{dir: dir, opts: options, schema: storedSchema, defs: defs, standalone: standalone}
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
	return open(dir, nil, opts...)
}

// OpenWithDefinitions opens a database-managed index from the authoritative
// schema. An old schema.bin is checked for compatibility if present, but it is
// not required and never becomes the runtime namespace registry.
func OpenWithDefinitions(dir string, definitions *schematypes.Definitions, opts ...Option) (*Store, error) {
	indexes, err := indexDefinitions(definitions)
	if err != nil {
		return nil, err
	}
	return open(dir, indexes, opts...)
}

func indexDefinitions(definitions *schematypes.Definitions) (*schematypes.Definitions, error) {
	if definitions == nil {
		return nil, ErrSchemaRequired
	}
	indexes := schematypes.NewDefinitions()
	for _, def := range definitions.Defs() {
		if def.Kind == schematypes.KindKV {
			continue
		}
		if err := indexes.Register(def); err != nil {
			return nil, err
		}
	}
	if _, err := validateSchema(indexes); err != nil {
		return nil, err
	}
	return indexes, nil
}

// ValidateDefinitions checks all index namespaces of an authoritative schema
// without creating physical engine files. It is used before publishing a
// prepared schema generation.
func ValidateDefinitions(definitions *schematypes.Definitions) error {
	_, err := indexDefinitions(definitions)
	return err
}

func open(dir string, supplied *schematypes.Definitions, opts ...Option) (*Store, error) {
	options := applyOptions(opts)
	schema, safe, flags, gens, err := readSchema(dir, supplied)
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
	s := &Store{dir: dir, opts: options, schema: schema, defs: defs, lock: lock, standalone: supplied == nil}
	// The flag is written before the first removal commits, so its absence
	// proves no removal was ever durable and the fast path is safe.
	s.liveRemovals.Store(flags&1 != 0)
	if err := s.openEngines(false); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	s.reconcileGenerations(gens, safe)
	if rec, rerr := readRecovery(dir); rerr != nil {
		_ = s.Close()
		return nil, rerr
	} else if len(rec) != 0 {
		// A prior interrupted multi-engine mutation may have left these
		// namespaces incomplete; they must not silently become healthy.
		s.pendingSet = rec
		for _, ns := range namespacesOfBitSet(rec) {
			if _, ok := s.schema.LookupIdentity(ns); ok {
				s.markDegraded(ns, ErrRecoveryRequired)
				s.health[ns].NeedsRebuild = true
			}
		}
	}
	return s, nil
}

// reconcileGenerations restores per-namespace completeness and validates it
// against each engine's own recoverable state. A namespace whose engine
// recovered an older watermark than the manifest recorded is stale, not
// corrupt: it is marked unavailable so a query cannot silently use it. The
// store-wide safe watermark is the minimum across declared namespaces.
func (s *Store) reconcileGenerations(gens []uint64, safe uint64) {
	for _, def := range s.defs {
		var recorded uint64
		if int(def.Namespace) < len(gens) {
			recorded = gens[def.Namespace]
		}
		if !s.health[def.Namespace].Available || s.isDegraded(def.Namespace) {
			s.nsSource[def.Namespace] = 0
			continue
		}
		if def.Kind == schematypes.KindTimestamp {
			idx, ok := s.reg.timeIndex(def.Namespace)
			if !ok {
				s.nsSource[def.Namespace] = 0
				continue
			}
			engineGen := idx.SourceGeneration()
			if engineGen < recorded {
				s.markUnavailable(def, fmt.Errorf("%w: namespace %d engine=%d recorded=%d", ErrIndexStale, def.Namespace, engineGen, recorded))
				s.nsSource[def.Namespace] = 0
				continue
			}
			s.nsSource[def.Namespace] = engineGen
			continue
		}
		s.nsSource[def.Namespace] = recorded
	}
	computed := s.recomputeSafe()
	if computed > safe {
		computed = safe
	}
	s.safeSource.Store(computed)
	// s.source tracks the highest generation any namespace has recorded, so
	// monotonicity survives reopen even when the store-wide safe watermark is
	// lower because some namespaces advanced independently.
	var maxGen uint64
	for _, def := range s.defs {
		if s.nsSource[def.Namespace] > maxGen {
			maxGen = s.nsSource[def.Namespace]
		}
	}
	s.source = maxGen
}

// recomputeSafe returns the minimum confirmed generation across declared
// namespaces. An unavailable namespace contributes zero, so the store-wide
// watermark never overstates completeness.
func (s *Store) recomputeSafe() uint64 {
	var safe uint64
	first := true
	for _, def := range s.defs {
		g := s.nsSource[def.Namespace]
		if first || g < safe {
			safe = g
			first = false
		}
	}
	return safe
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
	if s.standalone {
		schemaBytes, err := s.schema.MarshalBinary()
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.dir, schemaBinName, schemaBytes); err != nil {
			return err
		}
	}
	return s.writeManifest()
}

// RetireLegacySchema removes the old logical schema copy after schema.dat has
// been made authoritative. This is only valid for a database-managed opener;
// standalone Open still needs schema.bin to discover its definitions.
func (s *Store) RetireLegacySchema() error {
	if s.standalone || s.closed.Load() {
		return ErrInvalidSchema
	}
	path := filepath.Join(s.dir, schemaBinName)
	legacy, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	expected, err := s.schema.MarshalBinary()
	if err != nil {
		return err
	}
	if !bytes.Equal(legacy, expected) {
		return errors.Join(schematypes.ErrNamespace, ErrInvalidSchema)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return storage.SyncDir(s.dir)
}

// writeManifest persists the unified manifest: magic, version, namespace
// count, the store-wide safe watermark, one (namespace, generation) record per
// declared namespace, and a trailing CRC32C. It is called on Create and
// whenever a confirmed watermark advances.
func (s *Store) writeManifest() error {
	count := len(s.defs)
	buf := make([]byte, 0, 4+2+2+2+8+count*10+4)
	buf = binary.LittleEndian.AppendUint32(buf, storeMagic)
	buf = binary.LittleEndian.AppendUint16(buf, storeVersion)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(count))
	// flags bit 0: at least one liveness removal has been durably recorded.
	// It is written before the first removal commits, so its absence proves no
	// removal ever survived and the matcher's no-removal fast path is safe.
	var flags uint16
	if s.liveRemovals.Load() {
		flags = 1
	}
	buf = binary.LittleEndian.AppendUint16(buf, flags)
	buf = binary.LittleEndian.AppendUint64(buf, s.safeSource.Load())
	for _, def := range s.defs {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(def.Namespace))
		buf = binary.LittleEndian.AppendUint64(buf, s.nsSource[def.Namespace])
	}
	err := writeFileAtomic(s.dir, storeBinName, binary.LittleEndian.AppendUint32(buf, storage.Checksum(buf)))
	if err == nil {
		s.stats.manifestWrites.Add(1)
	}
	return err
}

// readSchema loads and validates the unified manifest and schema. It returns
// the schema, the recorded safe watermark, the header flags, and a dense
// per-namespace generation slice indexed by namespace number.
func readSchema(dir string, supplied *schematypes.Definitions) (*schematypes.Definitions, uint64, uint16, []uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, storeBinName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, nil, fmt.Errorf("%w: %s missing", ErrInvalidSchema, storeBinName)
		}
		return nil, 0, 0, nil, err
	}
	if len(raw) < 18+4 || binary.LittleEndian.Uint32(raw[0:4]) != storeMagic {
		return nil, 0, 0, nil, ErrCorrupt
	}
	if v := binary.LittleEndian.Uint16(raw[4:6]); v != storeVersion {
		return nil, 0, 0, nil, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, v)
	}
	if storage.Checksum(raw[:len(raw)-4]) != binary.LittleEndian.Uint32(raw[len(raw)-4:]) {
		return nil, 0, 0, nil, ErrCorrupt
	}
	count := int(binary.LittleEndian.Uint16(raw[6:8]))
	flags := binary.LittleEndian.Uint16(raw[8:10])
	safe := binary.LittleEndian.Uint64(raw[10:18])
	if flags&^uint16(1) != 0 {
		// Unknown persistent flags must not be silently ignored.
		return nil, 0, 0, nil, ErrCorrupt
	}
	body := raw[18 : len(raw)-4]
	if len(body) != count*10 {
		return nil, 0, 0, nil, ErrCorrupt
	}
	legacy, err := os.ReadFile(filepath.Join(dir, schemaBinName))
	if err != nil && (supplied == nil || !errors.Is(err, os.ErrNotExist)) {
		return nil, 0, 0, nil, err
	}
	var schema *schematypes.Definitions
	if supplied != nil {
		schema, err = cloneSchema(supplied)
		if err != nil {
			return nil, 0, 0, nil, err
		}
		if legacy != nil {
			current, err := schema.MarshalBinary()
			if err != nil {
				return nil, 0, 0, nil, err
			}
			if !bytes.Equal(legacy, current) {
				return nil, 0, 0, nil, errors.Join(schematypes.ErrNamespace, ErrInvalidSchema)
			}
		}
	} else {
		schema = schematypes.NewDefinitions()
		if err := schema.UnmarshalBinary(legacy); err != nil {
			return nil, 0, 0, nil, errors.Join(ErrCorrupt, err)
		}
	}
	defs := schema.Defs()
	// The generation table must describe exactly the schema's namespaces: no
	// missing records (incomplete table) and no unexpected ones.
	if count != len(defs) {
		return nil, 0, 0, nil, ErrCorrupt
	}
	var maxNS schematypes.Namespace
	for _, def := range defs {
		if def.Namespace > maxNS {
			maxNS = def.Namespace
		}
	}
	gens := make([]uint64, int(maxNS)+1)
	seen := make([]bool, int(maxNS)+1)
	for i := 0; i < count; i++ {
		ns := schematypes.Namespace(binary.LittleEndian.Uint16(body[i*10:]))
		gen := binary.LittleEndian.Uint64(body[i*10+2:])
		if _, ok := schema.Lookup(ns); !ok || int(ns) >= len(gens) || seen[ns] {
			return nil, 0, 0, nil, ErrCorrupt
		}
		seen[ns] = true
		gens[ns] = gen
	}
	for _, def := range defs {
		if !seen[def.Namespace] {
			return nil, 0, 0, nil, ErrCorrupt
		}
	}
	return schema, safe, flags, gens, nil
}

const (
	recoveryMagic   uint32 = 0x52454356 // RECV
	recoveryVersion uint16 = 1
	recoveryName           = "recovery.bin"
)

// persistRecovery writes or removes the durable recovery marker for the current
// pending set. The caller holds mu. An empty set removes the marker.
func (s *Store) persistRecovery() error {
	count := 0
	for _, w := range s.pendingSet {
		count += bits.OnesCount64(w)
	}
	if count == 0 {
		if err := os.Remove(filepath.Join(s.dir, recoveryName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return storage.SyncDir(s.dir)
	}
	buf := make([]byte, 0, 4+2+2+count*2+4)
	buf = binary.LittleEndian.AppendUint32(buf, recoveryMagic)
	buf = binary.LittleEndian.AppendUint16(buf, recoveryVersion)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(count))
	for w, word := range s.pendingSet {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) != 0 {
				buf = binary.LittleEndian.AppendUint16(buf, uint16(w*64+bit))
			}
		}
	}
	// The engines' contract is process-crash safety, not power-loss safety, so
	// the marker needs only an atomic rename in the page cache, not a directory
	// fsync. The rename keeps the marker from being observed torn.
	return writeFileNoSync(s.dir, recoveryName, binary.LittleEndian.AppendUint32(buf, storage.Checksum(buf)))
}

// writeFileNoSync writes data atomically via a temp file and rename but skips
// the directory fsync. It is used for the recovery marker, which must survive a
// process crash (write/rename are in the page cache) but not a power loss, to
// match the engines' documented durability contract at low cost.
func writeFileNoSync(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// readRecovery loads the recovery marker, returning nil when absent.
func readRecovery(dir string) ([]uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, recoveryName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) < 12 || binary.LittleEndian.Uint32(raw[0:4]) != recoveryMagic ||
		binary.LittleEndian.Uint16(raw[4:6]) != recoveryVersion {
		return nil, ErrCorrupt
	}
	if storage.Checksum(raw[:len(raw)-4]) != binary.LittleEndian.Uint32(raw[len(raw)-4:]) {
		return nil, ErrCorrupt
	}
	count := int(binary.LittleEndian.Uint16(raw[6:8]))
	body := raw[8 : len(raw)-4]
	if len(body) != count*2 {
		return nil, ErrCorrupt
	}
	var maxNS schematypes.Namespace
	for i := 0; i < count; i++ {
		if ns := schematypes.Namespace(binary.LittleEndian.Uint16(body[i*2:])); ns > maxNS {
			maxNS = ns
		}
	}
	if count == 0 {
		return nil, nil
	}
	set := make([]uint64, int(maxNS>>6)+1)
	for i := 0; i < count; i++ {
		ns := schematypes.Namespace(binary.LittleEndian.Uint16(body[i*2:]))
		set[ns>>6] |= 1 << (uint(ns) & 63)
	}
	return set, nil
}

// RecoveryNamespaces returns namespaces flagged by the durable recovery marker.
func (s *Store) RecoveryNamespaces() []schematypes.Namespace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return namespacesOfBitSet(s.pendingSet)
}

// ClearRecovery removes the durable recovery marker after the affected
// namespaces have been rebuilt. It does not re-enable namespaces already marked
// stale in this process; reopen after rebuilding.
func (s *Store) ClearRecovery() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	s.pendingSet = s.pendingSet[:0]
	return s.persistRecovery()
}

func namespacesOfBitSet(set []uint64) []schematypes.Namespace {
	var out []schematypes.Namespace
	for w, word := range set {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) != 0 {
				out = append(out, schematypes.Namespace(w*64+bit))
			}
		}
	}
	return out
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
	var maxNS schematypes.Namespace
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
	s.nsSource = make([]uint64, int(maxNS)+1)
	s.health = make([]NamespaceHealth, int(maxNS)+1)
	for i := range s.health {
		s.health[i].Namespace = schematypes.Namespace(i)
	}
	for e := schematypes.EntityImage; e.Valid(); e++ {
		s.reg.liveVal[e] = livenessValues[e]
	}

	if err := s.openBitmap(create); err != nil {
		s.markMatching(func(def schematypes.NamespaceDef) bool { return def.Kind.IsBitmap() }, err)
	} else {
		s.routeBitmaps()
	}
	if err := s.openTime(create); err != nil {
		s.markMatching(func(def schematypes.NamespaceDef) bool { return def.Kind == schematypes.KindTimestamp }, err)
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
		Kind:      schematypes.KindBitmapEnum,
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
		if def.Kind != schematypes.KindTimestamp {
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
		def, _ := s.schema.LookupIdentity(res.Namespace)
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
		if def.Kind != schematypes.KindGeo {
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
		if def.Kind != schematypes.KindHashPerceptual {
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
		if def.Kind != schematypes.KindHashExact {
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
		if def.Kind != schematypes.KindVector {
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
func (s *Store) attach(def schematypes.NamespaceDef, err error, ok func()) {
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

func (s *Store) setRoute(def schematypes.NamespaceDef, slot uint16) {
	s.reg.routes[def.Namespace] = route{kind: def.Kind, entity: def.Entity, slot: slot, available: true}
	h := &s.health[def.Namespace]
	h.Kind, h.Entity, h.Available = def.Kind, def.Entity, true
}

// markMatching records an engine-level failure against every matching
// namespace while leaving unrelated namespaces' health untouched.
func (s *Store) markMatching(match func(schematypes.NamespaceDef) bool, err error) {
	for _, def := range s.defs {
		if match(def) {
			s.markUnavailable(def, err)
		}
	}
}

func (s *Store) markUnavailable(def schematypes.NamespaceDef, err error) {
	h := &s.health[def.Namespace]
	h.Kind, h.Entity = def.Kind, def.Entity
	h.Err = err
	h.NeedsRebuild = needsRebuild(err)
	h.Corrupt = isCorrupt(err)
	if int(def.Namespace) < len(s.reg.routes) {
		s.reg.routes[def.Namespace].available = false
	}
}

func needsRebuild(err error) bool {
	return errors.Is(err, timeidx.ErrRebuildRequired) || errors.Is(err, schematypes.ErrCorruptSchema) || isCorrupt(err)
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

// Schema returns a defensive copy of the persisted schema. Mutating the result
// (including any NamespaceDef.Config bytes) cannot affect the Store.
func (s *Store) Schema() *schematypes.Definitions {
	clone := schematypes.NewDefinitions()
	if data, err := s.schema.MarshalBinary(); err == nil {
		_ = clone.UnmarshalBinary(data)
	}
	return clone
}

// NamespaceDef returns a copy of the persisted definition for a namespace. The
// Config bytes are deep-copied so callers cannot mutate stored configuration.
func (s *Store) NamespaceDef(ns schematypes.Namespace) (schematypes.NamespaceDef, bool) {
	def, ok := s.schema.Lookup(ns)
	if !ok {
		return schematypes.NamespaceDef{}, false
	}
	return def, true
}

// Health returns per-namespace open state for every declared namespace.
func (s *Store) Health() []NamespaceHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
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
func (s *Store) HealthOf(ns schematypes.Namespace) (NamespaceHealth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthOK || int(ns) >= len(s.health) {
		return NamespaceHealth{}, false
	}
	return s.health[ns], true
}

// NamespaceSourceGeneration returns the durable canonical completeness
// watermark for one declared namespace. It does not imply availability;
// callers must also inspect HealthOf before using a damaged index.
func (s *Store) NamespaceSourceGeneration(ns schematypes.Namespace) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schema.LookupIdentity(ns); !ok {
		return 0, false
	}
	return s.nsSource[ns], true
}

// isDegraded reports whether a namespace is known to be potentially incomplete
// after a partially failed batch. It is lock-free.
func (s *Store) isDegraded(ns schematypes.Namespace) bool {
	d := s.degraded.Load()
	if d == nil {
		return false
	}
	w := int(ns) >> 6
	if w >= len(*d) {
		return false
	}
	return (*d)[w]&(1<<(uint(ns)&63)) != 0
}

// markDegraded records that a namespace may be incomplete. The caller holds mu.
// It must not be called for a namespace that is already unavailable for another
// reason (health is left as-is in that case).
func (s *Store) markDegraded(ns schematypes.Namespace, err error) {
	words := (len(s.reg.routes) + 63) / 64
	cur := s.degraded.Load()
	var next []uint64
	if cur != nil {
		next = append([]uint64(nil), (*cur)...)
	}
	if len(next) < words {
		next = append(next, make([]uint64, words-len(next))...)
	}
	next[ns>>6] |= 1 << (uint(ns) & 63)
	s.degraded.Store(&next)
	if int(ns) < len(s.health) {
		h := &s.health[ns]
		h.Available = false
		h.Stale = true
		if err != nil && h.Err == nil {
			h.Err = err
		}
	}
}

// DegradedNamespaces returns the namespaces currently considered stale after a
// partially failed batch. They are unavailable until reopen or rebuild.
func (s *Store) DegradedNamespaces() []schematypes.Namespace {
	d := s.degraded.Load()
	if d == nil {
		return nil
	}
	var out []schematypes.Namespace
	for w, word := range *d {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) != 0 {
				out = append(out, schematypes.Namespace(w*64+bit))
			}
		}
	}
	return out
}

// SourceGeneration returns the confirmed store-wide canonical-database
// completeness watermark: the minimum confirmed generation across every
// declared namespace. Zero means unrecorded. It is read lock-free.
func (s *Store) SourceGeneration() uint64 { return s.safeSource.Load() }

// CheckSourceGeneration returns ErrIndexStale when the confirmed watermark is
// behind required, and ErrClosed after shutdown. Staleness is never corruption.
func (s *Store) CheckSourceGeneration(required uint64) error {
	if s.closed.Load() {
		return ErrClosed
	}
	if have := s.safeSource.Load(); have < required {
		return fmt.Errorf("%w: have %d, want %d", ErrIndexStale, have, required)
	}
	return nil
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

// Sync flushes every engine that exposes a synchronization barrier. It
// serializes with mutations and other lifecycle operations on the writer mutex.
func (s *Store) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// An interrupted batch can contain missing logical mutations even when
	// every surviving engine file is durable. Sync cannot resolve that marker.
	return err
}

// Checkpoint flushes every engine's durable checkpoint. It serializes with
// mutations on the writer mutex.
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// Checkpointing a partial state does not reconstruct omitted associations.
	return err
}

// Compact compacts every engine that supports it. It serializes with mutations
// on the writer mutex.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
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
func (s *Store) Time(ns schematypes.Namespace) (*timeidx.Index, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.timeIndex(ns)
}

// Geo returns a geographic namespace index if it is available.
func (s *Store) Geo(ns schematypes.Namespace) (*geoidx.Index, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.geoIndex(ns)
}

// Sim returns a perceptual-similarity namespace store if it is available.
func (s *Store) Sim(ns schematypes.Namespace) (*simhash.Store, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.simStore(ns)
}

// Hash returns an exact-hash namespace index if it is available.
func (s *Store) Hash(ns schematypes.Namespace) (*radixhash.Index, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.radixIndex(ns)
}

// Vector returns a vector namespace index if it is available.
func (s *Store) Vector(ns schematypes.Namespace) (*vectoridx.VectorIdx, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.vectorIndex(ns)
}

// Bitmap returns a bitmap namespace index if it is available.
func (s *Store) Bitmap(ns schematypes.Namespace) (*bitmapidx.Index, bool) {
	if s.isDegraded(ns) {
		return nil, false
	}
	return s.reg.bitmapIndex(ns)
}

// Liveness returns the entity liveness bitmap and its enum value.
func (s *Store) Liveness(entity schematypes.EntityKind) (*bitmapidx.Index, bitmapidx.Value, bool) {
	return s.reg.liveness(entity)
}

// BitmapStore returns the bitmap store owning every bitmap namespace, including
// the reserved liveness namespace. It backs ACL clause compilation.
func (s *Store) BitmapStore() *bitmapidx.Store { return s.bitmapStore }

// EntityOf returns the entity kind of a namespace.
func (s *Store) EntityOf(ns schematypes.Namespace) (schematypes.EntityKind, bool) {
	if int(ns) >= len(s.reg.routes) {
		return schematypes.EntityInvalid, false
	}
	rt := s.reg.routes[ns]
	if rt.slot == noSlot {
		return schematypes.EntityInvalid, false
	}
	return rt.entity, true
}
