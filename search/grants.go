package search

import (
	"errors"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

// ErrInvalidACLField reports a nil field or one that is not the dedicated
// KindEnum ACL field the handle requires.
var ErrInvalidACLField = errors.New("search: invalid ACL field")

// UserGrants manages direct image grants for the IndexTypeUserACL field:
// one posting per user holding the ImageIDs that user may see. Construct
// with NewUserGrants; the zero value is unusable.
type UserGrants = grantField[imageidx.UserID]

// NewUserGrants binds the store's user ACL field. It rejects a nil index
// and any field that is not the dedicated KindEnum user field.
func NewUserGrants(index *bitmapidx.FieldIndex) (UserGrants, error) {
	return newGrantField[imageidx.UserID](index, bitmapidx.IndexTypeUserACL)
}

// GroupGrants manages image grants for the IndexTypeGroupACL field: one
// posting per group holding the ImageIDs visible to its members. It also
// serves grant audits via the underlying field's ValueIDs. Construct with
// NewGroupGrants; the zero value is unusable.
type GroupGrants = grantField[imageidx.GroupID]

// NewGroupGrants binds the store's group ACL field. It rejects a nil index
// and any field that is not the dedicated KindEnum group field.
func NewGroupGrants(index *bitmapidx.FieldIndex) (GroupGrants, error) {
	return newGrantField[imageidx.GroupID](index, bitmapidx.IndexTypeGroupACL)
}

// grantField is the shared implementation behind UserGrants and
// GroupGrants. One 8-byte stack key serves the whole call: batch entries
// share it, and Value bytes are only borrowed through preparation —
// genuinely new dictionary values are copied under the writer lock — so
// helpers add no allocations of their own.
type grantField[T ~uint64] struct {
	index *bitmapidx.FieldIndex
}

// newGrantField validates index against the dedicated ACL field type.
func newGrantField[T ~uint64](index *bitmapidx.FieldIndex, field bitmapidx.IndexType) (grantField[T], error) {
	if index == nil {
		return grantField[T]{}, ErrInvalidACLField
	}
	def := index.Field()
	if def.Type != field || def.Kind != bitmapidx.KindEnum {
		return grantField[T]{}, ErrInvalidACLField
	}
	return grantField[T]{index: index}, nil
}

// value encodes id into a caller-owned key. The returned Value borrows key:
// callers must not mutate key until the index call returns, which holds
// for every helper below since each completes its call synchronously.
func principalValue(id uint64, key *[8]byte) bitmapidx.Value {
	*key = principalKey(id)
	return bitmapidx.Value{Kind: bitmapidx.KindEnum, Bytes: key[:]}
}

// Add grants each image to id in one atomic batch. Unknown images are
// created by granting; already-present memberships are no-ops. An empty
// image list is a no-op. Only the reserved zero id is rejected.
func (h grantField[T]) Add(id T, imageIDs ...ImageID) error {
	if id == 0 {
		return ErrInvalidPrincipal
	}
	if len(imageIDs) == 0 {
		return nil
	}
	// Small batches stay on the stack and share one key; larger ones take
	// the heap-backed path. The large helpers take the raw id (not the
	// encoded value) so the small path's key flows to exactly one sink and
	// escape analysis keeps it on the stack, matching hand-rolled calls.
	if len(imageIDs) > 8 {
		return h.addLarge(uint64(id), imageIDs)
	}
	var key [8]byte
	value := principalValue(uint64(id), &key)
	var inline [8]bitmapidx.Entry
	entries := inline[:len(imageIDs)]
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	return h.index.Add(entries...)
}

// addLarge grants a batch that exceeds the stack inline capacity. The one
// entries slice is inherent to batching: the equivalent hand-rolled
// FieldIndex call allocates it too.
func (h grantField[T]) addLarge(id uint64, imageIDs []ImageID) error {
	var key [8]byte
	value := principalValue(id, &key)
	entries := make([]bitmapidx.Entry, len(imageIDs))
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	return h.index.Add(entries...)
}

// Contains reports whether image holds id's grant. Unknown ids report
// false with no error; only the reserved zero id is rejected.
func (h grantField[T]) Contains(id T, image ImageID) (bool, error) {
	if id == 0 {
		return false, ErrInvalidPrincipal
	}
	var key [8]byte
	return h.index.Contains(image, principalValue(uint64(id), &key))
}

// Remove revokes each image from id in one atomic batch. Unknown revokes
// are no-ops and never create dictionary entries. An empty image list is
// a no-op. Only the reserved zero id is rejected.
func (h grantField[T]) Remove(id T, imageIDs ...ImageID) error {
	if id == 0 {
		return ErrInvalidPrincipal
	}
	if len(imageIDs) == 0 {
		return nil
	}
	if len(imageIDs) > 8 {
		return h.removeLarge(uint64(id), imageIDs)
	}
	var key [8]byte
	value := principalValue(uint64(id), &key)
	var inline [8]bitmapidx.Entry
	entries := inline[:len(imageIDs)]
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	return h.index.Remove(entries...)
}

// removeLarge revokes a batch that exceeds the stack inline capacity; see
// addLarge for the allocation note.
func (h grantField[T]) removeLarge(id uint64, imageIDs []ImageID) error {
	var key [8]byte
	value := principalValue(id, &key)
	entries := make([]bitmapidx.Entry, len(imageIDs))
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	return h.index.Remove(entries...)
}
