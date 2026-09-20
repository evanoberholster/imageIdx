package bitmapidx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
	"github.com/zeebo/xxh3"
)

// Kind is the value shape of a field. It selects hashing, storage and
// query behavior. Missing membership is distinct from an explicit Bool
// false. A field's kind is immutable for the life of its index and is part
// of the persisted definition.
type Kind uint8

const (
	KindEnum       Kind = iota + 1 // canonical discrete value, hashed
	KindBool                       // explicit false/true value
	KindText                       // analyzer tokens, each hashed
	KindContinuous                 // int64 grid key, order preserving
	KindGeo                        // S2 level-16 cell key, order preserving
)

// Storage is a reserved policy hint for a field. Both values are accepted
// and behave identically today: they share one lookup path and one mmap
// persistence layout. The distinction is persisted so a future cache policy
// can diverge without a format migration.
type Storage uint8

const (
	StoragePerValue Storage = iota + 1 // one lightweight handle per value
	StoragePerType                     // one field handle over mmap entries
)

// Normalizer codes for a field's enum canonicalization. Identity keeps
// caller-supplied bytes; Lowercase folds UTF-8 enum bytes with Unicode
// lowercase.
const (
	NormalizerIdentity  uint16 = 0 // caller-supplied canonical bytes
	NormalizerLowercase uint16 = 1 // Unicode lowercase for UTF-8 enum bytes
)

// Analyzer codec identifiers for Field.Analyzer. The Latin analyzer tokenizes
// Latin/whitespace text, lowercases it and folds accents.
const (
	AnalyzerLatin   uint16 = textanalyzer.CodeLatin
	AnalyzerVersion uint16 = textanalyzer.Version
)

// Quantizer codes identify the persisted conversion definition for a
// continuous field. The core API uses already-converted int64 grid values;
// the codec is descriptive so reopen can validate field definitions.
const (
	QuantIdentity uint16 = 1 // native integer domain, Scale 1, Offset 0
	QuantLinear   uint16 = 2 // grid = (value - Offset) / Scale
)

// CodecSpec identifies an immutable, versioned configuration. Params uses
// a deterministic encoding and is deep-copied at registration and on every
// Field return, so callers can never mutate registered state.
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

// clone returns a deep copy of the field definition, including codec
// params.
func (f Field) clone() Field {
	f.Normalizer = f.Normalizer.clone()
	f.Analyzer = f.Analyzer.clone()
	return f
}

// Quantizer describes conversion from source metadata into the int64 grid
// domain of a continuous field. The index API takes already-converted grid
// values; the quantizer is descriptive so reopen can validate that the
// writer and reader agree on the conversion.
type Quantizer struct {
	Code    uint16 // QuantIdentity, QuantLinear, ...
	Version uint16
	Scale   int64 // grid step in encoded units; must be > 0
	Offset  int64
}

// GeoCodec identifies the S2 cell level of a KindGeo field. Level zero
// selects the default GeoS2Level. Any other explicit level but GeoS2Level
// is rejected until smaller buckets are supported.
type GeoCodec struct {
	Version uint16
	Level   uint8
}

// Field is one registered metadata field: its stable type, value shape,
// reserved storage hint, and the codecs that interpret values. Only the
// codecs for the field's kind are consulted; the rest must be zero.
// Definitions are immutable once registered.
type Field struct {
	Type       IndexType
	Kind       Kind
	Storage    Storage
	Quant      Quantizer // KindContinuous only
	Geo        GeoCodec  // KindGeo only
	Normalizer CodecSpec // KindEnum only
	Analyzer   CodecSpec // KindText only
	Flags      uint32    // supported feature bits; unknown bits are rejected
}

const fieldKnownFlags uint32 = 0

var (
	// ErrInvalidField reports an invalid or unsupported field definition.
	ErrInvalidField = errors.New("bitmapidx: invalid field definition")
	// ErrFieldMismatch reports a value or definition that disagrees with the
	// registered field.
	ErrFieldMismatch = errors.New("bitmapidx: field definition mismatch")
	// ErrUnsupportedCodec reports an unknown or malformed codec version.
	ErrUnsupportedCodec = errors.New("bitmapidx: unsupported codec")
	// ErrHashCollision reports a hash match whose stored bytes disagree with
	// the expected value. Ordinary 64-bit hash collisions are resolved by
	// FieldKey.Slot; this error indicates corrupted dictionary state.
	ErrHashCollision = errors.New("bitmapidx: hash collision")
	// ErrTooManyCollisions reports exhausting the slot space for one hash.
	ErrTooManyCollisions = errors.New("bitmapidx: too many hash collisions for one key")
	// ErrValueKind reports a Value whose Kind does not match its field.
	ErrValueKind = errors.New("bitmapidx: value kind does not match field")
	// ErrValueTooLong reports canonical bytes above MaxValueBytes.
	ErrValueTooLong = errors.New("bitmapidx: value exceeds maximum length")
	// ErrInvalidRange reports an invalid numeric range or a range query on a
	// non-continuous field.
	ErrInvalidRange = errors.New("bitmapidx: invalid range")
	// ErrTooManyValues reports a mutation that would exceed the configured
	// distinct-value delta bound.
	ErrTooManyValues = errors.New("bitmapidx: too many distinct values in batch")
	// ErrUnsupportedOperation reports an operation a field kind does not
	// support, such as Add on a text field.
	ErrUnsupportedOperation = errors.New("bitmapidx: unsupported operation for field kind")
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
// MaxFieldBatchUpdates limit later; this bound only rejects batches that
// cannot possibly fit, before doing analyzer work.
const batchFanoutBound = 16

// validateField checks that f is a complete, supported definition: known
// kind and storage, valid codecs for the kind, sane quantizer and geo
// levels, and no unknown flags.
func validateField(f Field) error {
	if f.Type == 0 || f.Flags&^fieldKnownFlags != 0 {
		return ErrInvalidField
	}
	switch f.Kind {
	case KindEnum:
		if _, err := normalizeEnum(f.Normalizer, nil); err != nil {
			return err
		}
	case KindBool:
	case KindText:
		if _, err := textanalyzer.New(f.Analyzer.Code, f.Analyzer.Version, f.Analyzer.Params); err != nil {
			return errors.Join(ErrInvalidField, err)
		}
	case KindContinuous:
		if f.Quant.Code == 0 || f.Quant.Version == 0 || f.Quant.Scale <= 0 {
			return ErrInvalidField
		}
		if f.Quant.Code == QuantIdentity && (f.Quant.Scale != 1 || f.Quant.Offset != 0) {
			return ErrInvalidField
		}
	case KindGeo:
		if f.Geo.Version > 1 || (f.Geo.Level != 0 && f.Geo.Level != GeoS2Level) {
			return ErrInvalidField
		}
	default:
		return ErrInvalidField
	}
	switch f.Storage {
	case StoragePerValue, StoragePerType:
	default:
		return ErrInvalidField
	}
	return nil
}

// normalizeEnum canonicalizes enum bytes under the field normalizer.
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

// FieldKey identifies one value within a field.
//
// Enum, Bool, and Text keys carry a 64-bit xxh3 hash over the field type,
// kind, length, and canonical bytes. Continuous and Geo keys carry an
// order-preserving encoding in Hash with Slot zero: continuous stores the
// int64 grid value biased to uint64 order, geo stores the level-16 S2 key,
// so plain Hash order is numeric and geographic order respectively.
//
// Slot disambiguates 64-bit hash collisions: the first value seen for a
// hash claims slot zero and each colliding value claims the next free slot.
// Slot is always zero for continuous and geo keys, which never hash.
type FieldKey struct {
	Hash uint64
	Type IndexType
	Slot uint16
}

// orderKey maps a signed grid value onto an unsigned key whose natural
// order matches numeric order, so byte/uint64 sorts are numeric sorts.
func orderKey(grid int64) uint64 { return uint64(grid) ^ (1 << 63) }

// decodeOrderKey inverts orderKey.
func decodeOrderKey(key uint64) int64 { return int64(key ^ (1 << 63)) }

// continuousKey builds the FieldKey for one int64 grid value. The result
// always has Slot zero and Type t.
func continuousKey(t IndexType, grid int64) FieldKey {
	return FieldKey{Hash: orderKey(grid), Type: t}
}

// hashCanonical hashes canonical bytes with domain separation over the
// field type, kind, and length. It is a variable so tests can inject
// collisions by swapping in a constant hash. It is not safe for concurrent
// use: tests must swap it serially and restore it with Cleanup, and
// production code must never replace it.
var hashCanonical = hashCanonicalValue

func hashCanonicalValue(t IndexType, kind Kind, canonical []byte) FieldKey {
	var header [8]byte
	binary.LittleEndian.PutUint16(header[0:2], uint16(t))
	header[2] = byte(kind)
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
	return FieldKey{Hash: hash, Type: t}
}

// Value is one typed metadata value. The Kind discriminator must agree
// with the target field's Kind; every other arm is ignored. Bytes arms
// borrow caller memory through preparation and are copied under the writer
// lock only for genuinely new dictionary values.
type Value struct {
	Kind  Kind
	Bytes []byte
	Int   int64
	Bool  bool
}

// StringValue returns an enum value. The string is copied to a fresh byte
// slice; the caller retains its string.
func StringValue(value string) Value {
	return Value{Kind: KindEnum, Bytes: []byte(value)}
}

// BytesValue returns an enum value borrowing value. The slice is borrowed
// through preparation and copied under the writer lock only if the value
// is new to the dictionary; callers must not mutate it until the write
// returns.
func BytesValue(value []byte) Value {
	return Value{Kind: KindEnum, Bytes: value}
}

// BoolValue returns an explicit boolean value. False is distinct from
// absent: an image with no bool membership is neither true nor false.
func BoolValue(value bool) Value { return Value{Kind: KindBool, Bool: value} }

// IntValue returns a continuous value already converted into the field's
// int64 grid domain. No quantizer runs at write time.
func IntValue(value int64) Value { return Value{Kind: KindContinuous, Int: value} }

// GeoValue returns a KindGeo value carrying a level-16 S2 key. Keys fit in
// int64; build them with KeyFromLatLng or KeyFromCellID and query them
// with RangesFromCellIDs or CoverRegion.
func GeoValue(key uint64) Value { return Value{Kind: KindGeo, Int: int64(key)} }

// Entry is one ImageID to value membership change. ImageID zero is
// reserved and rejected with ErrInvalidImageID.
type Entry struct {
	ImageID ImageID
	Value   Value
}

// FieldMutation is one atomically applied membership batch. Remove wins
// over Add for the same (key, ImageID), both within the batch and against
// staged state. An empty mutation is a no-op.
type FieldMutation struct {
	Add    []Entry
	Remove []Entry
}

// Move is an explicit value transition for continuous and geo fields:
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

// TextReplacement replaces one image's complete logical text field.
// Callers supply the old and new source strings so the index can diff
// analyzer token sets without keeping a reverse index.
type TextReplacement struct {
	ImageID ImageID
	Before  []string
	After   []string
}

// Registry holds the field definitions known to a database. It is not
// safe for concurrent use: register all fields during setup before
// sharing. Every returned definition is a deep copy and shares no mutable
// state with the registry.
type Registry struct {
	fields map[IndexType]Field
}

// NewRegistry returns an empty field registry.
func NewRegistry() *Registry {
	return &Registry{fields: make(map[IndexType]Field)}
}

// Register adds validated field definitions. Re-registering an identical
// definition is idempotent; disagreeing definitions return ErrFieldMismatch.
func (r *Registry) Register(fields ...Field) error {
	for _, field := range fields {
		if err := validateField(field); err != nil {
			return err
		}
		field.Normalizer = field.Normalizer.clone()
		field.Analyzer = field.Analyzer.clone()
		if existing, ok := r.fields[field.Type]; ok {
			if !sameField(existing, field) {
				return ErrFieldMismatch
			}
			continue
		}
		r.fields[field.Type] = field
	}
	return nil
}

// Field returns a copy of the registered definition for a type.
func (r *Registry) Field(t IndexType) (Field, bool) {
	field, ok := r.fields[t]
	if !ok {
		return Field{}, false
	}
	return field.clone(), true
}

// Len returns the number of registered fields.
func (r *Registry) Len() int { return len(r.fields) }

// Fields returns copies of the registered definitions ordered by type.
func (r *Registry) Fields() []Field {
	out := make([]Field, 0, len(r.fields))
	for _, field := range r.fields {
		out = append(out, field.clone())
	}
	slices.SortFunc(out, func(a, b Field) int { return int(a.Type) - int(b.Type) })
	return out
}

// sameField reports whether two definitions are identical, including
// codec params.
func sameField(a, b Field) bool {
	if a.Type != b.Type || a.Kind != b.Kind || a.Storage != b.Storage ||
		a.Flags != b.Flags || a.Quant != b.Quant || a.Geo != b.Geo ||
		a.Normalizer.Code != b.Normalizer.Code || a.Normalizer.Version != b.Normalizer.Version ||
		!bytes.Equal(a.Normalizer.Params, b.Normalizer.Params) ||
		a.Analyzer.Code != b.Analyzer.Code || a.Analyzer.Version != b.Analyzer.Version ||
		!bytes.Equal(a.Analyzer.Params, b.Analyzer.Params) {
		return false
	}
	return true
}

const registryVersion uint16 = 1

// MarshalBinary encodes the registry deterministically by field type.
// The output carries registryVersion and round-trips through
// UnmarshalBinary, which re-validates every definition.
func (r *Registry) MarshalBinary() ([]byte, error) {
	fields := r.Fields()
	size := 2 + 4
	for _, field := range fields {
		size += 32 + 8 + len(field.Normalizer.Params) + 8 + len(field.Analyzer.Params)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint16(out, registryVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(fields)))
	for _, field := range fields {
		out = appendField(out, field)
	}
	return out, nil
}

// UnmarshalBinary replaces the registry from MarshalBinary output. It
// rejects trailing bytes, unknown versions, and definitions that fail
// validation.
func (r *Registry) UnmarshalBinary(data []byte) error {
	if len(data) < 6 || binary.LittleEndian.Uint16(data[0:2]) != registryVersion {
		return ErrInvalidField
	}
	count := binary.LittleEndian.Uint32(data[2:6])
	if uint64(count) > uint64(len(data)) {
		return ErrInvalidField
	}
	fields := make([]Field, 0, count)
	offset := 6
	for n := uint32(0); n < count; n++ {
		field, next, err := decodeField(data, offset)
		if err != nil {
			return err
		}
		fields = append(fields, field)
		offset = next
	}
	if offset != len(data) {
		return ErrInvalidField
	}
	next := NewRegistry()
	if err := next.Register(fields...); err != nil {
		return err
	}
	*r = *next
	return nil
}

func appendField(out []byte, field Field) []byte {
	out = binary.LittleEndian.AppendUint16(out, uint16(field.Type))
	out = append(out, byte(field.Kind), byte(field.Storage))
	out = binary.LittleEndian.AppendUint32(out, field.Flags)
	out = binary.LittleEndian.AppendUint16(out, field.Quant.Code)
	out = binary.LittleEndian.AppendUint16(out, field.Quant.Version)
	out = binary.LittleEndian.AppendUint64(out, uint64(field.Quant.Scale))
	out = binary.LittleEndian.AppendUint64(out, uint64(field.Quant.Offset))
	out = binary.LittleEndian.AppendUint16(out, field.Geo.Version)
	out = append(out, field.Geo.Level, 0)
	out = appendCodec(out, field.Normalizer)
	out = appendCodec(out, field.Analyzer)
	return out
}

func appendCodec(out []byte, spec CodecSpec) []byte {
	out = binary.LittleEndian.AppendUint16(out, spec.Code)
	out = binary.LittleEndian.AppendUint16(out, spec.Version)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(spec.Params)))
	out = append(out, spec.Params...)
	return out
}

func decodeField(data []byte, offset int) (Field, int, error) {
	if offset+32 > len(data) {
		return Field{}, 0, ErrInvalidField
	}
	field := Field{
		Type:    IndexType(binary.LittleEndian.Uint16(data[offset : offset+2])),
		Kind:    Kind(data[offset+2]),
		Storage: Storage(data[offset+3]),
		Flags:   binary.LittleEndian.Uint32(data[offset+4 : offset+8]),
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
		return Field{}, 0, err
	}
	analyzer, next, err := decodeCodec(data, next)
	if err != nil {
		return Field{}, 0, err
	}
	field.Normalizer = normalizer
	field.Analyzer = analyzer
	if err := validateField(field); err != nil {
		return Field{}, 0, err
	}
	return field, next, nil
}

func decodeCodec(data []byte, offset int) (CodecSpec, int, error) {
	if offset+8 > len(data) {
		return CodecSpec{}, 0, ErrInvalidField
	}
	spec := CodecSpec{
		Code:    binary.LittleEndian.Uint16(data[offset : offset+2]),
		Version: binary.LittleEndian.Uint16(data[offset+2 : offset+4]),
	}
	length := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
	if uint64(length) > uint64(len(data)-offset-8) {
		return CodecSpec{}, 0, ErrInvalidField
	}
	if length != 0 {
		spec.Params = append([]byte(nil), data[offset+8:offset+8+int(length)]...)
	}
	return spec, offset + 8 + int(length), nil
}
