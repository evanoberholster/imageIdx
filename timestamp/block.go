package timestamp

import "sync/atomic"

type TimestampBlockView struct {
	Base       *TimestampBase
	Frozen     *TimestampDelta
	Active     *TimestampDelta
	Generation uint64
}

type TimestampBlock struct {
	view atomic.Pointer[TimestampBlockView]

	// spare belongs to the write-owner goroutine. It is not published until a
	// rotation and is not reused until an epoch grace period has elapsed.
	spare *TimestampDelta
}

func newTimestampBlock(base *TimestampBase, deltaCapacity int) *TimestampBlock {
	block := &TimestampBlock{spare: newTimestampDelta(deltaCapacity)}
	block.view.Store(&TimestampBlockView{
		Base:       base,
		Active:     newTimestampDelta(deltaCapacity),
		Generation: 1,
	})
	return block
}

func (b *TimestampBlock) LoadView() *TimestampBlockView { return b.view.Load() }
