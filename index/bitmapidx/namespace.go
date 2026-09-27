package bitmapidx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
	"github.com/zeebo/xxh3"
)

// Storage is a reserved policy hint for a namespace. Both values are accepted
// and behave identically today: they share one lookup path and one mmap
// persistence layout. The distinction is persisted so a future cache policy
// can diverge without a format migration.
type Storage uint8

const (
	StoragePerValue Storage = iota + 1 // one lightweight handle per value
	StoragePerType                     // one namespace handle over mmap entries
)

// Normalizer codes for a namespace's enum canonicalization. Identity keeps
// caller-supplied bytes; Lowercase folds UTF-8 enum bytes with Unicode
// lowercase.
const (
	NormalizerIdentity  uint16 = 0 // caller-supplied canonical bytes
	NormalizerLowercase uint16 = 1 // Unicode lowercase for UTF-8 enum bytes
)

// Analyzer codec identifiers for NamespaceSpec.Analyzer. The Latin analyzer tokenizes
// Latin/whitespace text, lowercases it and folds accents.
const (
	AnalyzerLatin   uint16 = textanalyzer.CodeLatin
	AnalyzerVersion uint16 = textanalyzer.Version
)

// Quantizer codes identify the persisted conversion definition for a
// continuous namespace. The core API uses already-converted int64 grid values;
// the codec is descriptive so reopen can validate namespace definitions.
const (
	QuantIdentity uint16 = 1 // native integer domain, Scale 1, Offset 0
	QuantLinear   uint16 = 2 // grid = (value - Offset) / Scale
)

// CodecSpec identifies an immutable, versioned configuration. Params uses
// a deterministic encoding and is deep-copied at registration and on every
// NamespaceSpec return, so callers can never mutate registered state.
type CodecSpec struct {
	Code    uint16
	Version uint16
	Params  []byte
}

// clone returns a deep copy of the spec, including its params.
func (c CodecSpec) clone() CodecSpec {
	c.Params = append([]byte(nil), c.Params...)
	return c
}

// clone returns a deep copy of the namespace definition, including codec
// params.
func (f NamespaceSpec) clone() NamespaceSpec {
	f.Normalizer = f.Normalizer.clone()
	f.Analyzer = f.Analyzer.clone()
	return f
}

// Quantizer describes conversion from source metadata into the int64 grid
// domain of a continuous namespace. The index API takes already-converted grid
// values; the quantizer is descriptive so reopen can validate that the
// writer and reader agree on the conversion.
type Quantizer struct {
	Code    uint16 // QuantIdentity, QuantLinear, ...
	Version uint16
	Scale   int64 // grid step in encoded units; must be > 0
	Offset  int64
}

// GeoCodec identifies the S2 cell level of a KindBitmapGeo namespace. Level zero
// selects the default GeoS2Level. Any other explicit level but GeoS2Level
// is rejected until smaller buckets are supported.
type GeoCodec struct {
	Version uint16
	Level   uint8
}

// NamespaceSpec is one registered metadata namespace: its stable number and
// schema name, value shape, reserved storage hint, and the codecs that
// interpret values. Only the codecs for the namespace's kind are consulted;
// the rest must be zero. Definitions are immutable once registered. Name is
// schema metadata only and is never persisted to a bitmap segment.
type NamespaceSpec struct {
	Namespace  common.Namespace
	Name       string
	Kind       common.NamespaceKind
	Storage    Storage
	Quant      Quantizer // KindBitmapContinuous only
	Geo        GeoCodec  // KindBitmapGeo only
	Normalizer CodecSpec // KindBitmapEnum only
	Analyzer   CodecSpec // KindBitmapString only
	Flags      uint32    // supported feature bits; unknown bits are rejected
}

const namespaceKnownFlags uint32 = 0

var (
	// ErrInvalidNamespace reports an invalid or unsupported namespace
	// definition.
	ErrInvalidNamespace = errors.New("bitmapidx: invalid namespace definition")
	// ErrNamespaceMismatch reports a value or definition that disagrees with the
	// registered namespace.
	ErrNamespaceMismatch = errors.New("bitmapidx: namespace definition mismatch")
	// ErrUnsupportedCodec reports an unknown or malformed codec version.
	ErrUnsupportedCodec = errors.New("bitmapidx: unsupported codec")
	// ErrTooManyCollisions reports exhausting the slot space for one hash.
	ErrTooManyCollisions = errors.New("bitmapidx: too many hash collisions for one key")
	// ErrValueKind reports a Value whose Kind does not match its namespace.
	ErrValueKind = errors.New("bitmapidx: value kind does not match namespace")
	// ErrValueTooLong reports canonical bytes above MaxValueBytes.
	ErrValueTooLong = errors.New("bitmapidx: value exceeds maximum length")
	// ErrInvalidRange reports an invalid numeric range or a range query on a
	// non-continuous namespace.
	ErrInvalidRange = errors.New("bitmapidx: invalid range")
	// ErrUnsupportedOperation reports an operation a namespace kind does not
	// support, such as Add on a text namespace.
	ErrUnsupportedOperation = errors.New("bitmapidx: unsupported operation for namespace kind")
	// ErrInvalidCoordinate reports an out-of-domain geographic coordinate.
	ErrInvalidCoordinate = errors.New("bitmapidx: invalid geographic coordinate")
	// ErrInvalidRegion reports a nil geographic region.
	ErrInvalidRegion = errors.New("bitmapidx: invalid geographic region")
	// ErrInvalidS2Cell reports an invalid or wrong-level S2 cell.
	ErrInvalidS2Cell = errors.New("bitmapidx: invalid S2 cell")
	// ErrInvalidS2Level reports an unsupported S2 cell level.
	ErrInvalidS2Level = errors.New("bitmapidx: unsupported S2 level")
	// ErrTooManyCells reports a range expansion that would cover more S2
	// buckets than the caller's budget allows.
	ErrTooManyCells = errors.New("bitmapidx: range covers too many S2 cells")
	// ErrCoverWorkspaceFull reports insufficient cover workspace capacity.
	ErrCoverWorkspaceFull = errors.New("bitmapidx: cover workspace capacity is insufficient")
	// ErrCoverBufferFull reports insufficient cover destination capacity.
	ErrCoverBufferFull = errors.New("bitmapidx: cover destination capacity is insufficient")
)

// MaxValueBytes bounds one canonical value. Longer values are rejected
// before hashing or staging, so no oversized value ever reaches the
// dictionary, WAL, or segment.
const MaxValueBytes = 1 << 16

// batchFanoutBound is the conservative multiplier applied to entry counts
// in owner pre-checks. One Entry can fan out during preparation: a text
// replacement diffs Before/After through the analyzer and can stage many
// token memberships per entry. The WAL encoder enforces the exact
// MaxNamespaceBatchUpdates limit later; this bound only rejects batches that
// cannot possibly fit, before doing analyzer work.
const batchFanoutBound = 16

// validateNamespace checks that f is a complete, supported definition: known
// kind and storage, valid codecs for the kind, sane quantizer and geo
// levels, and no unknown flags.
func validateNamespace(f NamespaceSpec) error {
	if f.Namespace == 0 || f.Flags&^namespaceKnownFlags != 0 {
		return ErrInvalidNamespace
	}
	switch f.Kind {
	case common.KindBitmapEnum:
		if _, err := normalizeEnum(f.Normalizer, nil); err != nil {
			return err
		}
	case common.KindBitmapBool:
	case common.KindBitmapString:
		if _, err := textanalyzer.New(f.Analyzer.Code, f.Analyzer.Version, f.Analyzer.Params); err != nil {
			return errors.Join(ErrInvalidNamespace, err)
		}
	case common.KindBitmapContinuous:
		if f.Quant.Code == 0 || f.Quant.Version == 0 || f.Quant.Scale <= 0 {
			return ErrInvalidNamespace
		}
		if f.Quant.Code == QuantIdentity && (f.Quant.Scale != 1 || f.Quant.Offset != 0) {
			return ErrInvalidNamespace
		}
	case common.KindBitmapGeo:
		if f.Geo.Version > 1 || (f.Geo.Level != 0 && f.Geo.Level != GeoS2Level) {
			return ErrInvalidNamespace
		}
	default:
		return ErrInvalidNamespace
	}
	switch f.Storage {
	case StoragePerValue, StoragePerType:
	default:
		return ErrInvalidNamespace
	}
	return nil
}

// normalizeEnum canonicalizes enum bytes under the namespace normalizer.
// Identity returns the input slice borrowed; Lowercase allocates a
// lowercased copy. Unknown codes and versions fail with
// ErrUnsupportedCodec.
func normalizeEnum(spec CodecSpec, value []byte) ([]byte, error) {
	switch spec.Code {
	case NormalizerIdentity:
		if spec.Version != 0 && spec.Version != 1 {
			return nil, ErrUnsupportedCodec
		}
		return value, nil
	case NormalizerLowercase:
		if spec.Version != 1 {
			return nil, ErrUnsupportedCodec
		}
		return bytes.ToLower(value), nil
	default:
		return nil, ErrUnsupportedCodec
	}
}

// NamespaceKey identifies one value within a namespace.
//
// Enum, Bool, and Text keys carry a 64-bit xxh3 hash over the namespace,
// kind, length, and canonical bytes. Continuous and Geo keys carry an
// order-preserving encoding in Hash with Slot zero: continuous stores the
// int64 grid value biased to uint64 order, geo stores the level-16 S2 key,
// so plain Hash order is numeric and geographic order respectively.
//
// Slot disambiguates 64-bit hash collisions: the first value seen for a
// hash claims slot zero and each colliding value claims the next free slot.
// Slot is always zero for continuous and geo keys, which never hash.
type NamespaceKey struct {
	Hash      uint64
	Namespace common.Namespace
	Slot      uint16
}

// orderKey maps a signed grid value onto an unsigned key whose natural
// order matches numeric order, so byte/uint64 sorts are numeric sorts.
func orderKey(grid int64) uint64 { return uint64(grid) ^ (1 << 63) }

// decodeOrderKey inverts orderKey.
func decodeOrderKey(key uint64) int64 { return int64(key ^ (1 << 63)) }

// continuousKey builds the NamespaceKey for one int64 grid value. The result
// always has Slot zero and Namespace t.
func continuousKey(t common.Namespace, grid int64) NamespaceKey {
	return NamespaceKey{Hash: orderKey(grid), Namespace: t}
}

// hashCanonical hashes canonical bytes with domain separation over the
// namespace, kind, and length. It is a variable so tests can inject
// collisions by swapping in a constant hash. It is not safe for concurrent
// use: tests must swap it serially and restore it with Cleanup, and
// production code must never replace it.
var hashCanonical = hashCanonicalValue

func hashCanonicalValue(t common.Namespace, kind common.NamespaceKind, canonical []byte) NamespaceKey {
	var header [8]byte
	binary.LittleEndian.PutUint16(header[0:2], uint16(t))
	header[2] = bitmapKindCode(kind)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(canonical)))

	var scratch [520]byte
	var hash uint64
	if len(canonical) <= len(scratch)-8 {
		copy(scratch[:8], header[:])
		copy(scratch[8:], canonical)
		hash = xxh3.Hash(scratch[:8+len(canonical)])
	} else {
		buf := make([]byte, 8+len(canonical))
		copy(buf[:8], header[:])
		copy(buf[8:], canonical)
		hash = xxh3.Hash(buf)
	}
	return NamespaceKey{Hash: hash, Namespace: t}
}

// Value is one typed metadata value. The Kind discriminator must agree
// with the target namespace's Kind; every other arm is ignored. Bytes arms
// borrow caller memory through preparation and are copied under the writer
// lock only for genuinely new dictionary values.
type Value struct {
	Kind  common.NamespaceKind
	Bytes []byte
	Int   int64
	Bool  bool
}

// StringValue returns an enum value. The string is copied to a fresh byte
// slice; the caller retains its string.
func StringValue(value string) Value {
	return Value{Kind: common.KindBitmapEnum, Bytes: []byte(value)}
}

// BytesValue returns an enum value borrowing value. The slice is borrowed
// through preparation and copied under the writer lock only if the value
// is new to the dictionary; callers must not mutate it until the write
// returns.
func BytesValue(value []byte) Value {
	return Value{Kind: common.KindBitmapEnum, Bytes: value}
}

// BoolValue returns an explicit boolean value. False is distinct from
// absent: an image with no bool membership is neither true nor false.
func BoolValue(value bool) Value { return Value{Kind: common.KindBitmapBool, Bool: value} }

// IntValue returns a continuous value already converted into the namespace's
// int64 grid domain. No quantizer runs at write time.
func IntValue(value int64) Value { return Value{Kind: common.KindBitmapContinuous, Int: value} }

// GeoValue returns a KindBitmapGeo value carrying a level-16 S2 key. Keys fit in
// int64; build them with KeyFromLatLng or KeyFromCellID and query them
// with RangesFromCellIDs or CoverRegion.
func GeoValue(key uint64) Value { return Value{Kind: common.KindBitmapGeo, Int: int64(key)} }

// Entry is one ImageID to value membership change. ImageID zero is
// reserved and rejected with ErrInvalidImageID.
type Entry struct {
	ImageID ImageID
	Value   Value
}

// namespaceMutation is one atomically prepared membership batch. Remove wins
// over Add for the same (key, ImageID), both within the batch and against
// staged state. An empty mutation is a no-op.
type namespaceMutation struct {
	Add    []Entry
	Remove []Entry
}

// Move is an explicit value transition for continuous and geo namespaces:
// HasFrom=false inserts, HasTo=false removes, both set moves. Flags
// resolve the zero-value ambiguity. Continuous keeps explicit flags
// because zero is a valid grid value; geo may omit them because a
// level-16 S2 key is never zero, so zero is the absent sentinel.
type Move struct {
	ImageID ImageID
	From    int64
	To      int64
	HasFrom bool
	HasTo   bool
}

// TextReplacement replaces one image's complete logical text namespace.
// Callers supply the old and new source strings so the index can diff
// analyzer token sets without keeping a reverse index.
type TextReplacement struct {
	ImageID ImageID
	Before  []string
	After   []string
}

// SchemaDef converts the spec into its common.Schema definition, encoding
// the codecs into Config with the persisted namespace layout. Name is carried
// on the definition only and is never written into a bitmap segment.
func (s NamespaceSpec) SchemaDef() (common.NamespaceDef, error) {
	if err := validateNamespace(s); err != nil {
		return common.NamespaceDef{}, err
	}
	return common.NamespaceDef{
		Namespace: s.Namespace,
		Name:      s.Name,
		Kind:      s.Kind,
		Config:    appendNamespace(nil, s),
	}, nil
}

// NamespaceSpecFromSchemaDef is the inverse of SchemaDef. It rejects
// definitions that do not describe a bitmap namespace with ErrNamespaceMismatch.
func NamespaceSpecFromSchemaDef(def common.NamespaceDef) (NamespaceSpec, error) {
	if !def.Kind.IsBitmap() {
		return NamespaceSpec{}, ErrNamespaceMismatch
	}
	spec, next, err := decodeNamespace(def.Config, 0)
	if err != nil {
		return NamespaceSpec{}, err
	}
	if next != len(def.Config) || spec.Namespace != def.Namespace || def.Kind != spec.Kind {
		return NamespaceSpec{}, ErrNamespaceMismatch
	}
	spec.Name = def.Name
	return spec, nil
}

// NewNamespaceSchema converts specs into a common.Schema. Every spec must
// have a non-empty name and a valid namespace; duplicate namespace numbers
// and duplicate names are rejected by common.Schema.RegisterAll.
func NewNamespaceSchema(specs ...NamespaceSpec) (*common.Schema, error) {
	defs := make([]common.NamespaceDef, 0, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			return nil, common.ErrNamespaceName
		}
		def, err := spec.SchemaDef()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	schema := common.NewSchema()
	if err := schema.RegisterAll(defs...); err != nil {
		return nil, err
	}
	return schema, nil
}

// validateNamespaceSpecs validates each definition and rejects duplicate
// namespace numbers. NewIndex uses it to keep store definitions unique
// without requiring schema names, which are not persisted.
func validateNamespaceSpecs(specs []NamespaceSpec) error {
	seen := make(map[common.Namespace]struct{}, len(specs))
	for _, spec := range specs {
		if err := validateNamespace(spec); err != nil {
			return err
		}
		if _, ok := seen[spec.Namespace]; ok {
			return fmt.Errorf("%w: %d", common.ErrNamespaceDuplicate, spec.Namespace)
		}
		seen[spec.Namespace] = struct{}{}
	}
	return nil
}

const registryVersion uint16 = 1

// encodeNamespaceSpecs encodes specs deterministically by namespace number.
// The layout is the byte-identical persisted registry format: registryVersion,
// count, then each spec's namespace, kind, storage, flags, quantizer, geo
// codec, and normalizer/analyzer codecs. Names are never persisted.
func encodeNamespaceSpecs(specs []NamespaceSpec) []byte {
	sorted := slices.Clone(specs)
	slices.SortFunc(sorted, func(a, b NamespaceSpec) int { return int(a.Namespace) - int(b.Namespace) })
	size := 2 + 4
	for _, spec := range sorted {
		size += 32 + 8 + len(spec.Normalizer.Params) + 8 + len(spec.Analyzer.Params)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint16(out, registryVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(sorted)))
	for _, spec := range sorted {
		out = appendNamespace(out, spec)
	}
	return out
}

// decodeNamespaceSpecs decodes and validates encodeNamespaceSpecs output.
// It rejects unknown versions, trailing bytes, invalid definitions, and
// duplicate namespace numbers.
func decodeNamespaceSpecs(data []byte) ([]NamespaceSpec, error) {
	if len(data) < 6 || binary.LittleEndian.Uint16(data[0:2]) != registryVersion {
		return nil, ErrInvalidNamespace
	}
	count := binary.LittleEndian.Uint32(data[2:6])
	if uint64(count) > uint64(len(data)) {
		return nil, ErrInvalidNamespace
	}
	specs := make([]NamespaceSpec, 0, count)
	offset := 6
	for n := uint32(0); n < count; n++ {
		spec, next, err := decodeNamespace(data, offset)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
		offset = next
	}
	if offset != len(data) {
		return nil, ErrInvalidNamespace
	}
	if err := validateNamespaceSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// bitmapKindCode keeps the original bitmap wire and hash domain codes stable.
func bitmapKindCode(kind common.NamespaceKind) byte {
	switch kind {
	case common.KindBitmapEnum:
		return 1
	case common.KindBitmapBool:
		return 2
	case common.KindBitmapString:
		return 3
	case common.KindBitmapContinuous:
		return 4
	case common.KindBitmapGeo:
		return 5
	default:
		return 0
	}
}

func bitmapKindFromCode(code byte) common.NamespaceKind {
	switch code {
	case 1:
		return common.KindBitmapEnum
	case 2:
		return common.KindBitmapBool
	case 3:
		return common.KindBitmapString
	case 4:
		return common.KindBitmapContinuous
	case 5:
		return common.KindBitmapGeo
	default:
		return 0
	}
}

func appendNamespace(out []byte, namespace NamespaceSpec) []byte {
	out = binary.LittleEndian.AppendUint16(out, uint16(namespace.Namespace))
	out = append(out, bitmapKindCode(namespace.Kind), byte(namespace.Storage))
	out = binary.LittleEndian.AppendUint32(out, namespace.Flags)
	out = binary.LittleEndian.AppendUint16(out, namespace.Quant.Code)
	out = binary.LittleEndian.AppendUint16(out, namespace.Quant.Version)
	out = binary.LittleEndian.AppendUint64(out, uint64(namespace.Quant.Scale))
	out = binary.LittleEndian.AppendUint64(out, uint64(namespace.Quant.Offset))
	out = binary.LittleEndian.AppendUint16(out, namespace.Geo.Version)
	out = append(out, namespace.Geo.Level, 0)
	out = appendCodec(out, namespace.Normalizer)
	out = appendCodec(out, namespace.Analyzer)
	return out
}

func appendCodec(out []byte, spec CodecSpec) []byte {
	out = binary.LittleEndian.AppendUint16(out, spec.Code)
	out = binary.LittleEndian.AppendUint16(out, spec.Version)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(spec.Params)))
	out = append(out, spec.Params...)
	return out
}

func decodeNamespace(data []byte, offset int) (NamespaceSpec, int, error) {
	if offset+32 > len(data) {
		return NamespaceSpec{}, 0, ErrInvalidNamespace
	}
	namespace := NamespaceSpec{
		Namespace: common.Namespace(binary.LittleEndian.Uint16(data[offset : offset+2])),
		Kind:      bitmapKindFromCode(data[offset+2]),
		Storage:   Storage(data[offset+3]),
		Flags:     binary.LittleEndian.Uint32(data[offset+4 : offset+8]),
		Quant: Quantizer{
			Code:    binary.LittleEndian.Uint16(data[offset+8 : offset+10]),
			Version: binary.LittleEndian.Uint16(data[offset+10 : offset+12]),
			Scale:   int64(binary.LittleEndian.Uint64(data[offset+12 : offset+20])),
			Offset:  int64(binary.LittleEndian.Uint64(data[offset+20 : offset+28])),
		},
		Geo: GeoCodec{
			Version: binary.LittleEndian.Uint16(data[offset+28 : offset+30]),
			Level:   data[offset+30],
		},
	}
	normalizer, next, err := decodeCodec(data, offset+32)
	if err != nil {
		return NamespaceSpec{}, 0, err
	}
	analyzer, next, err := decodeCodec(data, next)
	if err != nil {
		return NamespaceSpec{}, 0, err
	}
	namespace.Normalizer = normalizer
	namespace.Analyzer = analyzer
	if err := validateNamespace(namespace); err != nil {
		return NamespaceSpec{}, 0, err
	}
	return namespace, next, nil
}

func decodeCodec(data []byte, offset int) (CodecSpec, int, error) {
	if offset+8 > len(data) {
		return CodecSpec{}, 0, ErrInvalidNamespace
	}
	spec := CodecSpec{
		Code:    binary.LittleEndian.Uint16(data[offset : offset+2]),
		Version: binary.LittleEndian.Uint16(data[offset+2 : offset+4]),
	}
	length := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
	if uint64(length) > uint64(len(data)-offset-8) {
		return CodecSpec{}, 0, ErrInvalidNamespace
	}
	if length != 0 {
		spec.Params = append([]byte(nil), data[offset+8:offset+8+int(length)]...)
	}
	return spec, offset + 8 + int(length), nil
}
