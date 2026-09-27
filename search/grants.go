package search

import (
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

// ErrInvalidACLField reports a nil namespace or one that is not the dedicated
// KindBitmapEnum ACL namespace the handle requires.
var ErrInvalidACLField = errors.New("search: invalid ACL namespace")

// UserGrants manages direct image grants for the NamespaceUserACL namespace:
// one posting per user holding the ImageIDs that user may see. Construct
// with NewUserGrants; the zero value is unusable.
type UserGrants = grantNamespace[common.UserID]

// NewUserGrants binds the store's user ACL namespace. It rejects a nil index
// and any namespace that is not the dedicated KindBitmapEnum user namespace.
func NewUserGrants(index *bitmapidx.Index) (UserGrants, error) {
	return newGrantNamespace[common.UserID](index, common.NamespaceUserACL)
}

// GroupGrants manages image grants for the NamespaceGroupACL namespace: one
// posting per group holding the ImageIDs visible to its members. It also
// serves grant audits via the underlying namespace's ValueIDs. Construct with
// NewGroupGrants; the zero value is unusable.
type GroupGrants = grantNamespace[common.GroupID]

// NewGroupGrants binds the store's group ACL namespace. It rejects a nil index
// and any namespace that is not the dedicated KindBitmapEnum group namespace.
func NewGroupGrants(index *bitmapidx.Index) (GroupGrants, error) {
	return newGrantNamespace[common.GroupID](index, common.NamespaceGroupACL)
}

// grantNamespace is the shared implementation behind UserGrants and
// GroupGrants. One 8-byte stack key serves the whole call: batch entries
// share it, and Value bytes are only borrowed through Apply —
// genuinely new dictionary values are copied under the writer lock — so
// helpers add no allocations of their own beyond the batch itself.
type grantNamespace[T ~uint64] struct {
	index *bitmapidx.Index
}

// newGrantNamespace validates index against the dedicated ACL namespace.
func newGrantNamespace[T ~uint64](index *bitmapidx.Index, namespace common.Namespace) (grantNamespace[T], error) {
	if index == nil {
		return grantNamespace[T]{}, ErrInvalidACLField
	}
	def := index.Spec()
	if def.Namespace != namespace || def.Kind != common.KindBitmapEnum {
		return grantNamespace[T]{}, ErrInvalidACLField
	}
	return grantNamespace[T]{index: index}, nil
}

// value encodes id into a caller-owned key. The returned Value borrows key:
// callers must not mutate key until Apply returns, which holds for every
// helper below since staging and Apply complete synchronously in the call.
func principalValue(id uint64, key *[8]byte) bitmapidx.Value {
	*key = principalKey(id)
	return bitmapidx.Value{Kind: common.KindBitmapEnum, Bytes: key[:]}
}

// Add grants each image to id in one atomic batch. Unknown images are
// created by granting; already-present memberships are no-ops. An empty
// image list is a no-op. Only the reserved zero id is rejected.
func (h grantNamespace[T]) Add(id T, imageIDs ...ImageID) error {
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
	batch := h.index.NewBatch()
	defer batch.Close()
	if err := batch.Add(entries...); err != nil {
		return err
	}
	if err := batch.Apply(); err != nil {
		return err
	}
	return h.index.Barrier()
}

// addLarge grants a batch that exceeds the stack inline capacity. The one
// entries slice is inherent to batching: the equivalent hand-rolled
// Index call allocates it too.
func (h grantNamespace[T]) addLarge(id uint64, imageIDs []ImageID) error {
	var key [8]byte
	value := principalValue(id, &key)
	entries := make([]bitmapidx.Entry, len(imageIDs))
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	batch := h.index.NewBatch()
	defer batch.Close()
	if err := batch.Add(entries...); err != nil {
		return err
	}
	if err := batch.Apply(); err != nil {
		return err
	}
	return h.index.Barrier()
}

// Contains reports whether image holds id's grant. Unknown ids report
// false with no error; only the reserved zero id is rejected.
func (h grantNamespace[T]) Contains(id T, image ImageID) (bool, error) {
	if id == 0 {
		return false, ErrInvalidPrincipal
	}
	var key [8]byte
	return h.index.Contains(image, principalValue(uint64(id), &key))
}

// Remove revokes each image from id in one atomic batch. Unknown revokes
// are no-ops and never create dictionary entries. An empty image list is
// a no-op. Only the reserved zero id is rejected.
func (h grantNamespace[T]) Remove(id T, imageIDs ...ImageID) error {
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
	batch := h.index.NewBatch()
	defer batch.Close()
	if err := batch.Remove(entries...); err != nil {
		return err
	}
	if err := batch.Apply(); err != nil {
		return err
	}
	return h.index.Barrier()
}

// removeLarge revokes a batch that exceeds the stack inline capacity; see
// addLarge for the allocation note.
func (h grantNamespace[T]) removeLarge(id uint64, imageIDs []ImageID) error {
	var key [8]byte
	value := principalValue(id, &key)
	entries := make([]bitmapidx.Entry, len(imageIDs))
	for n, image := range imageIDs {
		entries[n] = bitmapidx.Entry{ImageID: image, Value: value}
	}
	batch := h.index.NewBatch()
	defer batch.Close()
	if err := batch.Remove(entries...); err != nil {
		return err
	}
	if err := batch.Apply(); err != nil {
		return err
	}
	return h.index.Barrier()
}
