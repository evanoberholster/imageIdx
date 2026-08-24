package timestamp

import "sync/atomic"

type blockView struct {
	base       *base
	frozen     *delta
	active     *delta
	generation uint64
}

type block struct {
	view atomic.Pointer[blockView]

	// spare belongs to the write-owner goroutine. It is not published until a
	// rotation and is not reused until a read-side grace period has elapsed.
	spare *delta
}

func newBlock(base *base, deltaCapacity int) *block {
	block := &block{spare: newDelta(deltaCapacity)}
	block.view.Store(&blockView{
		base:       base,
		active:     newDelta(deltaCapacity),
		generation: 1,
	})
	return block
}
