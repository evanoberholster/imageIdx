package common

// Namespace identifies one indexed collection: a bitmap namespace, a
// timestamp index, a hash index, a vector space, or a key-value collection.
// Namespaces are unique within a Schema, and zero is reserved and never valid.
type Namespace uint16

// InvalidNamespace is the reserved zero namespace.
const InvalidNamespace Namespace = 0

// Valid reports whether ns is a usable namespace.
func (ns Namespace) Valid() bool { return ns != InvalidNamespace }

// Well-known bitmap namespaces. Applications may allocate further namespaces
// with Schema.Register as long as numbers and names stay unique.
const (
	NamespaceLocation Namespace = iota + 1
	NamespacePerson
	NamespaceAlbum
	NamespaceTag
	NamespaceObject
	NamespaceEvent

	// v3 additions
	NamespaceRating
	NamespaceColorLabel
	NamespaceFlag

	// Reserved bitmap namespaces for access-control postings: one enum
	// namespace per principal kind, so the stored key IS the uint64 user or
	// group ID in fixed 8-byte form. Principal encoding and policy live in
	// search; the index packages store opaque values.
	NamespaceUserACL
	NamespaceGroupACL
)

// NamespaceKind identifies the index family and, for bitmap indexes, the
// value shape of a namespace.
type NamespaceKind uint8

const (
	// Bitmap kinds use a separate range so existing schema kind numbers remain
	// stable. Reserved values are not valid namespace kinds.
	KindBitmapEnum       NamespaceKind = 0x11 // canonical discrete value
	KindBitmapBool       NamespaceKind = 0x12 // explicit false/true value
	KindBitmapString     NamespaceKind = 0x13 // analyzer tokens
	KindBitmapContinuous NamespaceKind = 0x14 // int64 grid key
	KindBitmapGeo        NamespaceKind = 0x15 // S2 cell key
	KindBitmapBSI        NamespaceKind = 0x16 // Bit-Sliced Index (BSI) for int64 values, including floats
	KindBitmapReserved1  NamespaceKind = 0x17 // reserved for future bitmap kinds
	KindBitmapReserved2  NamespaceKind = 0x18

	// KindTimestamp is a timeidx timestamp index.
	KindTimestamp NamespaceKind = 2
	// KindHashExact is a hashidx exact-content 256-bit digest index.
	KindHashExact NamespaceKind = 3
	// KindHashPerceptual is a hashidx perceptual index (pHash64 and PDQ).
	KindHashPerceptual NamespaceKind = 4
	// KindVector is a vectoridx approximate-nearest-neighbor space.
	KindVector NamespaceKind = 5
	// KindKV is a Pebble-backed key-value collection.
	KindKV NamespaceKind = 6
)

// String returns a stable human-readable kind name.
func (k NamespaceKind) String() string {
	switch k {
	case KindTimestamp:
		return "timestamp"
	case KindHashExact:
		return "hash-exact"
	case KindHashPerceptual:
		return "hash-perceptual"
	case KindVector:
		return "vector"
	case KindKV:
		return "kv"
	case KindBitmapEnum:
		return "bitmap-enum"
	case KindBitmapBool:
		return "bitmap-bool"
	case KindBitmapString:
		return "bitmap-string"
	case KindBitmapContinuous:
		return "bitmap-continuous"
	case KindBitmapGeo:
		return "bitmap-geo"
	case KindBitmapBSI:
		return "bitmap-bsi"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a recognized namespace kind.
func (k NamespaceKind) Valid() bool {
	return k >= KindTimestamp && k <= KindKV ||
		k >= KindBitmapEnum && k <= KindBitmapBSI
}

// IsBitmap reports whether k is a bitmap namespace kind.
func (k NamespaceKind) IsBitmap() bool {
	return k >= KindBitmapEnum && k <= KindBitmapBSI
}
