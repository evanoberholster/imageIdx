package bitmapidx

import (
	"sync"

	"github.com/evanoberholster/imageIdx/roar"
)

// Pooled scratch bounds. Buffers that grow beyond these caps are dropped
// instead of returned, so one huge query cannot pin megabytes process-wide.
const (
	maxPooledPostings = 4096
	maxPooledIDs      = 1 << 20
	maxPooledBitmaps  = 1024
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
	mutablePool = sync.Pool{New: func() any {
		return roar.NewMutableBitmap()
	}}
)

// acquirePostings returns a scratch pointer slice sized for n entries. The
// caller must release it; contents are only valid until release.
func acquirePostings(n int) *[]*posting {
	p := postingPtrPool.Get().(*[]*posting)
	if cap(*p) < n {
		*p = make([]*posting, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releasePostings returns scratch, dropping oversized buffers.
func releasePostings(p *[]*posting) {
	if cap(*p) > maxPooledPostings {
		return
	}
	*p = (*p)[:0]
	postingPtrPool.Put(p)
}

// acquireIDs returns a scratch ID buffer sized for n entries.
func acquireIDs(n int) *[]uint64 {
	p := idSlicePool.Get().(*[]uint64)
	if cap(*p) < n {
		*p = make([]uint64, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releaseIDs returns scratch, dropping oversized buffers. The released slice
// must not alias any retained bitmap: callers encode first, then release.
func releaseIDs(p *[]uint64) {
	if cap(*p) > maxPooledIDs {
		return
	}
	*p = (*p)[:0]
	idSlicePool.Put(p)
}

// acquireBitmaps returns a scratch bitmap slice sized for n entries.
func acquireBitmaps(n int) *[]roar.Bitmap {
	p := bitmapSlicePool.Get().(*[]roar.Bitmap)
	if cap(*p) < n {
		*p = make([]roar.Bitmap, 0, n)
	} else {
		*p = (*p)[:0]
	}
	return p
}

// releaseBitmaps returns scratch, dropping oversized buffers.
func releaseBitmaps(p *[]roar.Bitmap) {
	if cap(*p) > maxPooledBitmaps {
		return
	}
	*p = (*p)[:0]
	bitmapSlicePool.Put(p)
}

// acquireMutable returns a reset accumulator for one caller's exclusive use.
func acquireMutable() *roar.MutableBitmap {
	m := mutablePool.Get().(*roar.MutableBitmap)
	m.Reset()
	return m
}

// releaseMutable returns the accumulator. Callers must materialize first via
// AppendTo plus FromUnsorted; a Freeze result aliases pooled scratch and must
// never outlive the call.
func releaseMutable(m *roar.MutableBitmap) {
	mutablePool.Put(m)
}
