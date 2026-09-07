package idset

import (
	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/roar"
)

// Cursor accelerates repeated Contains probes against one immutable Ref when
// probes are non-decreasing or have locality. It returns the same results as
// Contains for any order.
//
// A Cursor is not safe for concurrent use. It must not outlive the Ref's
// backing bytes, and the Ref must not be modified or freed while the cursor
// is live. Create one Cursor per query worker, never share it.
type Cursor struct {
	cursor roar.Cursor
}

// NewCursor returns a cursor bound to ref. It allocates nothing.
func (ref Ref) NewCursor() Cursor {
	return Cursor{cursor: ref.bitmap.NewCursor()}
}

// Contains reports whether id is present. It is correct for any probe order.
func (c *Cursor) Contains(id imageidx.ImageID) bool {
	return c.cursor.Contains(id)
}
