package bitmapidx

import "github.com/evanoberholster/imageIdx/roar"

// ownResult detaches a query result from store-owned mmap state. Base
// postings may alias the mapped segment, which a checkpoint can unmap; a
// store-owned index therefore clones every result so callers never observe
// unmapped memory. Fresh unions already own their bytes, so the clone is a
// deliberate safety-for-copy tradeoff: one extra copy per query on
// persistent stores in exchange for no lifetime coupling to checkpoints.
func (i *FieldIndex) ownResult(result *roar.Bitmap) {
	if i.owner != nil {
		*result = result.Clone()
	}
}
