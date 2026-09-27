package bitmapidx

import "github.com/evanoberholster/imageIdx/roar"

// detachOwned clones a query result when it could alias store-owned mmap
// state. Only a result whose bytes live in the mapped segment is not
// heap-owned; corrected postings, tiny postings, and fresh unions and
// intersections are heap-owned and are returned without copying. This keeps
// the lifetime safety of the old unconditional clone while removing a full
// posting copy from every query that materialized its own result.
func (i *Index) detachOwned(result *roar.Bitmap, owned bool) {
	if i.owner != nil && !owned {
		*result = result.Clone()
	}
}
