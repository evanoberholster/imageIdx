package bitmapidx

import (
	"sync"

	"github.com/evanoberholster/imageIdx/roar"
)

// Pooled scratch for query paths. Buffers that grow beyond these caps are
// dropped instead of returned, so one huge query cannot pin megabytes
// process-wide. Pools hold slice headers only; bitmap payloads stay owned
// by their postings or by freshly built results.
//
// Contract: every acquire must pair with its release. Contents are valid
// only until release. Callers must not retain an acquired slice after
// releasing it.
const (
	maxPooledPostings = 4096
	maxPooledIDs      = 1 << 20
	maxPooledBitmaps  = 1024
	maxPooledKeys     = 4096
)

var (
	postingPtrPool = sync.Pool{New: func() any {
		s := make([]*posting, 0, 64)
		return &s
	}}
	idSlicePool = sync.Pool{New: func() any {
		s := make([]uint64, 0, 1024)
		return &s
	}}
	bitmapSlicePool = sync.Pool{New: func() any {
		s := make([]roar.Bitmap, 0, 64)
		return &s
	}}
	fieldKeySlicePool = sync.Pool{New: func() any {
		s := make([]FieldKey, 0, 64)
		return &s
	}}
)

// acquirePostings returns a scratch pointer slice with capacity for at
// least n entries, reset to length zero.
func acquirePostings(n int) *[]*posting {
	p := postingPtrPool.Get().(*[]*posting)
	if cap(*p) < n {
		*p = make([]*posting, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releasePostings returns scratch to the pool, dropping buffers whose
// capacity exceeds maxPooledPostings. Retained posting pointers are
// cleared first so the pooled backing array cannot pin obsolete postings.
func releasePostings(p *[]*posting) {
	if cap(*p) > maxPooledPostings {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	postingPtrPool.Put(p)
}

// acquireIDs returns a scratch ID buffer with capacity for at least n
// entries, reset to length zero.
func acquireIDs(n int) *[]uint64 {
	p := idSlicePool.Get().(*[]uint64)
	if cap(*p) < n {
		*p = make([]uint64, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releaseIDs returns scratch to the pool, dropping buffers whose capacity
// exceeds maxPooledIDs. The released slice must not alias any retained
// bitmap: callers must materialize (for example via roar.FromUnsorted,
// which copies unless the input is already strictly increasing) before
// releasing.
func releaseIDs(p *[]uint64) {
	if cap(*p) > maxPooledIDs {
		return
	}
	*p = (*p)[:0]
	idSlicePool.Put(p)
}

// acquireBitmaps returns a scratch bitmap slice with capacity for at least
// n entries, reset to length zero. The slice header is pooled; the bitmap
// payloads it points at stay owned by their postings or results.
func acquireBitmaps(n int) *[]roar.Bitmap {
	p := bitmapSlicePool.Get().(*[]roar.Bitmap)
	if cap(*p) < n {
		*p = make([]roar.Bitmap, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releaseBitmaps returns scratch to the pool, dropping buffers whose
// capacity exceeds maxPooledBitmaps. Retained bitmap payloads are cleared
// first so the pooled backing array cannot pin obsolete bitmaps.
func releaseBitmaps(p *[]roar.Bitmap) {
	if cap(*p) > maxPooledBitmaps {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	bitmapSlicePool.Put(p)
}

// acquireFieldKeys returns a scratch FieldKey buffer with capacity for at
// least n entries, reset to length zero.
func acquireFieldKeys(n int) *[]FieldKey {
	p := fieldKeySlicePool.Get().(*[]FieldKey)
	if cap(*p) < n {
		*p = make([]FieldKey, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releaseFieldKeys returns scratch to the pool, dropping buffers whose
// capacity exceeds maxPooledKeys. FieldKey carries no pointers, so no
// clearing is required.
func releaseFieldKeys(p *[]FieldKey) {
	if cap(*p) > maxPooledKeys {
		return
	}
	*p = (*p)[:0]
	fieldKeySlicePool.Put(p)
}
