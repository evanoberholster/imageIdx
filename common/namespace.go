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

// NamespaceKind identifies the index family that owns a namespace.
type NamespaceKind uint8

const (
	// KindBitmap is a bitmapidx namespace: enum, bool, text, continuous, or
	// geographic values over image IDs.
	KindBitmap NamespaceKind = iota + 1
	// KindTimestamp is a timeidx timestamp index.
	KindTimestamp
	// KindHashExact is a hashidx exact-content 256-bit digest index.
	KindHashExact
	// KindHashPerceptual is a hashidx perceptual index (pHash64 and PDQ).
	KindHashPerceptual
	// KindVector is a vectoridx approximate-nearest-neighbor space.
	KindVector
	// KindKV is a Pebble-backed key-value collection.
	KindKV
)

// String returns a stable human-readable kind name.
func (k NamespaceKind) String() string {
	switch k {
	case KindBitmap:
		return "bitmap"
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
	default:
		return "unknown"
	}
}

// Valid reports whether k is a recognized namespace kind.
func (k NamespaceKind) Valid() bool { return k >= KindBitmap && k <= KindKV }
