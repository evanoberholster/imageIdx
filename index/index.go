// Package index provides a unified, namespace-aware Store over the six imageIdx
// index families: timeidx (KindTimestamp), geoidx (KindGeo), simhash
// (KindHashPerceptual), radixhash (KindHashExact), vectoridx (KindVector), and
// bitmapidx (the KindBitmap* kinds).
//
// A Store owns every underlying engine instance for one root directory. Every
// namespace is declared in a common.Schema before Create/Open; namespaces are
// never registered at runtime. Routing is a dense, allocation-free numeric
// table, and every hot path avoids maps and reflection.
//
// # Consistency contract
//
// A Store is NOT a single atomic multi-engine transaction. A batch is applied
// engine by engine under the Store's write mutex. Value mutations are published
// first; entity liveness is updated last for additions and first for deletions,
// so a partially failed batch never makes an incomplete entity searchable.
// Individual engines keep their own crash-safe, copy-on-write commit and reopen
// guarantees, and every engine is independently rebuildable from the canonical
// application database. See Store.Apply and the package README for details.
package index

import (
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// Sentinel errors. They are wrapped with context where useful.
var (
	// ErrSchemaRequired reports a nil schema.
	ErrSchemaRequired = errors.New("index: schema is required")
	// ErrInvalidSchema reports a duplicate, empty, or unsupported namespace.
	ErrInvalidSchema = errors.New("index: invalid schema")
	// ErrEntityRequired reports a namespace without a valid entity kind.
	ErrEntityRequired = errors.New("index: namespace requires a valid entity kind")
	// ErrEntityMismatch reports an operation on the wrong entity kind.
	ErrEntityMismatch = errors.New("index: entity kind mismatch")
	// ErrNamespaceNotFound reports an unknown or unrouted namespace.
	ErrNamespaceNotFound = errors.New("index: namespace not found")
	// ErrNamespaceUnavailable reports a namespace that failed to open.
	ErrNamespaceUnavailable = errors.New("index: namespace unavailable")
	// ErrWrongEngine reports a value routed to the wrong index family.
	ErrWrongEngine = errors.New("index: value does not match namespace family")
	// ErrClosed reports use of a closed Store.
	ErrClosed = errors.New("index: store is closed")
	// ErrBatchClosed reports reuse of a batch that is being applied.
	ErrBatchClosed = errors.New("index: batch is in flight or closed")
	// ErrValueIDTooLarge reports a ValueID that cannot be stored in a uint32
	// vector NodeID without truncation.
	ErrValueIDTooLarge = errors.New("index: ValueID exceeds the uint32 vector node range")
	// ErrCorrupt reports a corrupt unified manifest or schema.
	ErrCorrupt = errors.New("index: corrupt unified store")
	// ErrUnsupportedVersion reports a unified manifest from a newer writer.
	ErrUnsupportedVersion = errors.New("index: unsupported unified store version")
	// ErrEmptyBatch reports Apply with no staged operations.
	ErrEmptyBatch = errors.New("index: batch has no operations")
	// ErrDuplicateNamespace reports a duplicate namespace number.
	ErrDuplicateNamespace = errors.New("index: duplicate namespace")
	// ErrIndexStale reports that the store's confirmed completeness watermark is
	// behind a required canonical-database generation. It is a logical staleness
	// signal, never corruption.
	ErrIndexStale = errors.New("index: index is behind the authoritative database")
	// ErrNamespaceIDModel reports a ValueID incompatible with an engine's
	// native identifier model. simhash assigns dense, 1-based record IDs, so a
	// simhash value must use the next dense ValueID.
	ErrNamespaceIDModel = errors.New("index: ValueID incompatible with namespace id model")
)

// Spec declares the namespace schema for a new Store. Every namespace must be
// registered before Create; the schema is persisted and validated on Open.
type Spec struct {
	Schema *common.Schema
}

// Options carries runtime policy shared by all namespaces. Persisted
// per-namespace configuration lives in common.NamespaceDef.Config instead.
type Options struct {
	// Durability selects the durability policy passed to engines that expose
	// one (simhash, vectoridx). timeidx always commits synchronously.
	Durability common.Durability
	// RadixArenaBytes is the mmap arena size for each radixhash namespace.
	RadixArenaBytes uint64
	// Vector configures each vector namespace. Dimensions/ElementType/Metric are
	// authoritative in the namespace schema; other fields are runtime policy.
	Vector vectoridx.Config
}

func (o Options) withDefaults() Options {
	if !o.Durability.Valid() {
		o.Durability = common.DurabilityOS
	}
	if o.RadixArenaBytes == 0 {
		o.RadixArenaBytes = 64 << 20
	}
	return o
}

// Option mutates Options.
type Option func(*Options)

// WithDurability sets the durability policy for engines that support one.
func WithDurability(d common.Durability) Option {
	return func(o *Options) { o.Durability = d }
}

// WithRadixArenaBytes sets the mmap arena size for radixhash namespaces.
func WithRadixArenaBytes(n uint64) Option {
	return func(o *Options) { o.RadixArenaBytes = n }
}

// TimeValue is one timestamp for the given timestamp namespace.
type TimeValue struct {
	Namespace common.Namespace
	Timestamp uint64
}

// GeoValue is one S2 geographic key for the given geo namespace.
type GeoValue struct {
	Namespace common.Namespace
	Key       geoidx.S2Key
}

// SimValue is one perceptual-hash record for the given similarity namespace.
type SimValue struct {
	Namespace common.Namespace
	Record    simhash.Record
}

// HashValue is one exact 256-bit hash for the given exact-hash namespace.
type HashValue struct {
	Namespace common.Namespace
	Hash      radixhash.Hash
}

// VectorValue is one vector for the given vector namespace. Exactly the slice
// matching Element is used; the others must be nil. It carries the element
// slices directly so callers avoid interface boxing on the mutation path.
type VectorValue struct {
	Namespace common.Namespace
	Element   vectoridx.ElementType
	Float32   []float32
	Int8      []int8
	Uint8     []uint8
	Int16     []int16
	Uint16    []uint16
}

// BitmapValue is one typed bitmap value for the given bitmap namespace.
type BitmapValue struct {
	Namespace common.Namespace
	Value     bitmapidx.Value
}

// Entry is a set of index associations for one entity. The entity kind is
// implied by each value's namespace schema, never stored per record.
type Entry struct {
	ValueID common.ValueID

	Times   []TimeValue
	Geos    []GeoValue
	Sims    []SimValue
	Hashes  []HashValue
	Vectors []VectorValue
	Bitmaps []BitmapValue
}

// Change is a partial update: the Before and After entries must describe the
// same entity ValueID. A namespace present in either side is reconciled by set
// difference (values removed from Before that are absent in After are
// removed; new After values are added). A namespace absent from both sides is
// untouched. This makes explicit removal unambiguous while keeping unrelated
// namespaces write-free.
type Change struct {
	Before Entry
	After  Entry
}

// NamespaceHealth reports the open state of one namespace. Stale is set when a
// failed multi-engine batch may have left the namespace partially applied; such
// a namespace is treated as unavailable until the process is reopened (each
// engine is individually consistent after reopen) or the namespace is rebuilt.
type NamespaceHealth struct {
	Namespace    common.Namespace
	Kind         common.NamespaceKind
	Entity       common.EntityKind
	Available    bool
	NeedsRebuild bool
	Corrupt      bool
	Stale        bool
	Err          error
}

// Metadata summarizes the Store.
type Metadata struct {
	Dir              string
	Namespaces       int
	SourceGeneration uint64
	Generation       uint64
}
