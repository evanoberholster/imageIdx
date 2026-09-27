package hashidx

import (
	"sync"

	common "github.com/evanoberholster/imageIdx/common"
)

// This file implements generation-gated compiled point lookups,
// the ExactIndex analog of bitmapidx's compiled Query with its dictGen
// fast path: resolve once against the current view, then replay the
// cached result while the view generation is unchanged.

// CompiledLookup is a reusable exact-hash point lookup. The hash is fixed
// at construction; the first execution resolves the sorted unique ImageIDs
// against the current view (including any staged overlay) and every later
// execution replays the owned copy while the view generation is stable.
// A generation change re-resolves once. It holds no query scratch beyond
// the cached IDs and is safe for concurrent use.
type CompiledLookup struct {
	index *ExactIndex
	hash  Hash
	mu    sync.Mutex
	gen   uint64
	ids   []common.ImageID
	ok    bool
}

// CompileLookup resolves hash to a reusable lookup handle. Resolution is
// lazy: nothing runs until the first Contains, Count, or Lookup call, so
// compiling against a changing index never observes a torn view.
func (i *ExactIndex) CompileLookup(hash Hash) *CompiledLookup {
	return &CompiledLookup{index: i, hash: hash}
}

// ensure resolves the handle against view, reusing the cache while the
// generation matches. The caller must hold c.mu; reader tracking follows
// the same acquire/release discipline as the one-shot paths.
func (c *CompiledLookup) ensure() {
	i := c.index
	if i == nil {
		return
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() && view != nil {
		if !c.ok || view.generation != c.gen {
			// Reuses the cached backing when capacity suffices, so
			// steady-state re-resolution after a generation change
			// allocates only when the run outgrows it.
			c.ids = viewLookup(c.ids[:0], view, c.hash)
			c.gen = view.generation
			c.ok = true
		}
	} else {
		// Mirror the one-shot paths: closed indexes report empty.
		// Closed is terminal, so dropping the cache is safe.
		c.ids = c.ids[:0]
		c.ok = false
	}
	i.readers.Release(token)
}

// Contains reports whether any ImageID is associated with the compiled
// hash.
func (c *CompiledLookup) Contains() bool {
	if c == nil || c.index == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	return c.ok && len(c.ids) > 0
}

// Count returns the number of ImageIDs associated with the compiled hash
// without materializing.
func (c *CompiledLookup) Count() uint64 {
	if c == nil || c.index == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	if !c.ok {
		return 0
	}
	return uint64(len(c.ids))
}

// Lookup appends the sorted unique ImageIDs for the compiled hash to dst.
// The cached copy is never aliased: results append out of it.
func (c *CompiledLookup) Lookup(dst []common.ImageID) []common.ImageID {
	if c == nil || c.index == nil {
		return dst
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	if !c.ok {
		return dst
	}
	return append(dst, c.ids...)
}
