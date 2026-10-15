package schema

// EntityKind identifies the kind of entity an indexed value describes. It is a
// property of the namespace schema, never of an individual ValueID: two entity
// kinds may share the same numeric ValueID and remain logically independent.
//
// The numbering is part of the persisted schema and must stay stable.
type EntityKind uint8

const (
	// EntityInvalid is the reserved zero value and means "unspecified". The
	// unified index Store rejects namespaces without a valid entity before it
	// persists or opens a schema.
	EntityInvalid EntityKind = iota
	EntityImage
	EntityFace
	EntityPerson
	EntityObject
	EntityAlbum
	EntityEvent
	EntityPlace
	EntityAsset
	EntityCollection
	EntityStack
	// Added in allocator format v2. Never renumber persisted entity kinds.
	EntityUser
	EntityLocation
	EntityTag

	entityKindCount // sentinel; not a valid kind
)

// Valid reports whether e is one of the supported entity kinds.
func (e EntityKind) Valid() bool { return e >= EntityImage && e < entityKindCount }

// String returns a stable lower-case name used for diagnostics and for the
// internal liveness namespace values. Unknown kinds return "invalid".
func (e EntityKind) String() string {
	switch e {
	case EntityImage:
		return "image"
	case EntityFace:
		return "face"
	case EntityPerson:
		return "person"
	case EntityObject:
		return "object"
	case EntityAlbum:
		return "album"
	case EntityEvent:
		return "event"
	case EntityPlace:
		return "place"
	case EntityAsset:
		return "asset"
	case EntityCollection:
		return "collection"
	case EntityStack:
		return "stack"
	case EntityUser:
		return "user"
	case EntityLocation:
		return "location"
	case EntityTag:
		return "tag"
	default:
		return "invalid"
	}
}

// EntityKindCount is the number of valid entity kinds (excluding
// EntityInvalid). It sizes fixed per-entity tables without a lookup map.
const EntityKindCount = int(entityKindCount) - 1

// EntityID aliases per entity kind. Every one is the shared 64-bit ValueID;
// the aliases exist for readability at call sites and never change storage.
// ImageID is declared alongside ValueID in types.go.
type (
	FaceID       = ValueID
	PersonID     = ValueID
	ObjectID     = ValueID
	AlbumID      = ValueID
	EventID      = ValueID
	PlaceID      = ValueID
	AssetID      = ValueID
	CollectionID = ValueID
	StackID      = ValueID
	LocationID   = ValueID
	TagID        = ValueID
)
