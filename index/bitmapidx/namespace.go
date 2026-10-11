package bitmapidx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/textanalyzer"
	"github.com/zeebo/xxh3"
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

// BSI (bit-sliced index) widths. A BSI namespace maps each ValueID to one
// unsigned numeric value of the declared width. Only these widths are valid;
// BSI16 and BSI32 are configurations of one namespace kind, not separate
// kinds.
const (
	// BSIWidth16 is a 16-bit (uint16) BSI namespace.
	BSIWidth16 uint8 = 16
	// BSIWidth32 is a 32-bit (uint32) BSI namespace.
	BSIWidth32 uint8 = 32
)

// NamespaceSpec is one registered metadata namespace: its stable number,
// value shape, and the codecs that interpret values. Only the codecs for the
// namespace's kind are consulted; the rest must be zero. Definitions are
// immutable once registered. Schema names belong to common.NamespaceDef.
//
// Geography is not a bitmap kind: geographic indexing lives in index/geoidx.
type NamespaceSpec struct {
	Namespace  common.Namespace
	Kind       common.NamespaceKind
	Name       string    // Namespace Name; not persisted, only for user display
	BSIWidth   uint8     // KindBitmapBSI only: BSIWidth16 or BSIWidth32
	Quant      Quantizer // KindBitmapContinuous only
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
	// ErrInvalidBSIWidth reports a BSI width other than 16 or 32.
	ErrInvalidBSIWidth = errors.New("bitmapidx: BSI width must be 16 or 32")
	// ErrBSIValueRange reports a value that does not fit the namespace BSI
	// width. Values are never silently truncated.
	ErrBSIValueRange = errors.New("bitmapidx: value out of range for BSI width")
	// ErrBSIStale reports a conditional BSI update whose expected old value no
	// longer matches the published value.
	ErrBSIStale = errors.New("bitmapidx: stale BSI from value")
	// ErrBSIExists reports a conditional BSI insert whose ValueID already holds
	// a value.
	ErrBSIExists = errors.New("bitmapidx: BSI insert on existing value")
	// ErrBSIDuplicate reports the same ValueID appearing more than once in one
	// BSI batch, which has ambiguous sequential semantics.
	ErrBSIDuplicate = errors.New("bitmapidx: duplicate BSI value ID in batch")
	// ErrBSINotBSI reports a BSI operation on a namespace of another kind.
	ErrBSINotBSI = errors.New("bitmapidx: namespace is not a BSI namespace")
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
	// Only BSI namespaces carry a width; every other kind must leave it zero so
	// a mismatched persisted width is rejected rather than silently ignored.
	if f.Kind == common.KindBitmapBSI {
		if f.BSIWidth != BSIWidth16 && f.BSIWidth != BSIWidth32 {
			return ErrInvalidBSIWidth
		}
	} else if f.BSIWidth != 0 {
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
	case common.KindBitmapBSI:
		// Width is validated above.
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

// EnumStringValue returns an enum value. The string is copied to a fresh byte
// slice; the caller retains its string.
func EnumStringValue(value string) Value {
	return Value{Kind: common.KindBitmapEnum, Bytes: []byte(value)}
}

// EnumBytesValue returns an enum value borrowing value. The slice is borrowed
// through preparation and copied under the writer lock only if the value
// is new to the dictionary; callers must not mutate it until the write
// returns.
func EnumBytesValue(value []byte) Value { return Value{Kind: common.KindBitmapEnum, Bytes: value} }

// BoolValue returns an explicit boolean value. False is distinct from
// absent: an image with no bool membership is neither true nor false.
func BoolValue(value bool) Value { return Value{Kind: common.KindBitmapBool, Bool: value} }

// IntValue returns a continuous value already converted into the namespace's
// int64 grid domain. No quantizer runs at write time.
func IntValue(value int64) Value { return Value{Kind: common.KindBitmapContinuous, Int: value} }

// Uint16Value returns a 16-bit BSI value. It is only valid for a BSI namespace
// whose width is BSIWidth16; the writer rejects it against any other width.
func Uint16Value(value uint16) Value { return Value{Kind: common.KindBitmapBSI, Int: int64(value)} }

// Uint32Value returns a 32-bit BSI value. It is only valid for a BSI namespace
// whose width is BSIWidth32; the writer rejects it against any other width.
func Uint32Value(value uint32) Value { return Value{Kind: common.KindBitmapBSI, Int: int64(value)} }

// BSIMove is one logical BSI value transition for a ValueID. The presence
// flags are literal preconditions:
//
//   - HasFrom and HasTo: replace, requiring the current value to equal From.
//   - HasFrom only: delete, requiring the current value to equal From.
//   - HasTo only: insert, requiring the ValueID to be absent.
//   - neither: a no-op.
//
// A no-op move is dropped. Values must fit the namespace width; they are never
// truncated. All IDs are common.ValueID: a BSI namespace is entity-agnostic.
type BSIMove struct {
	ID      common.ValueID
	From    uint32
	To      uint32
	HasFrom bool
	HasTo   bool
}

// Entry is one ImageID to value membership change. ImageID zero is
// reserved and rejected with ErrInvalidImageID.
type Entry struct {
	ImageID common.ImageID
	Value   Value
}

// namespaceMutation is one atomically prepared membership batch. Remove wins
// over Add for the same (key, ImageID), both within the batch and against
// staged state. An empty mutation is a no-op.
type namespaceMutation struct {
	Add    []Entry
	Remove []Entry
}

// Move is a value transition for continuous and geo namespaces. From == 0
// inserts, To == 0 removes, and both nonzero values move. Both zero is a no-op.
// A continuous value of zero must use Batch.Add or Batch.Remove instead.
type Move struct {
	ImageID common.ImageID
	From    int64
	To      int64
}

// TextReplacement replaces one image's complete logical text namespace.
// Callers supply the old and new source strings so the index can diff
// analyzer token sets without keeping a reverse index.
type TextReplacement struct {
	ImageID common.ImageID
	Before  []string
	After   []string
}

// NamespaceDef converts the spec into a common.Schema definition, encoding
// the codecs into Config with the persisted namespace layout.
func (s NamespaceSpec) NamespaceDef(name string) (common.NamespaceDef, error) {
	if name == "" {
		return common.NamespaceDef{}, common.ErrNamespaceName
	}
	if err := validateNamespace(s); err != nil {
		return common.NamespaceDef{}, err
	}
	return common.NamespaceDef{
		Namespace: s.Namespace,
		Name:      name,
		Kind:      s.Kind,
		Config:    appendNamespace(nil, s),
	}, nil
}

// NamespaceSpecFromSchemaDef is the inverse of NamespaceDef. It rejects
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
	return spec, nil
}

// validateNamespaceSpecs validates each definition and rejects duplicate
// namespace numbers. NewStore uses it to keep store definitions unique
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
