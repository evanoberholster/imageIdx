package timestamp

import "sync/atomic"

type barrierResult struct {
	walOffset int64
	err       error
}

type TimestampWriteBatch struct {
	Writes []TimestampWrite

	done     chan error
	inFlight atomic.Bool
}

func NewWriteBatch(capacity int) *TimestampWriteBatch {
	return &TimestampWriteBatch{
		Writes: make([]TimestampWrite, 0, capacity),
		done:   make(chan error, 1),
	}
}

func (b *TimestampWriteBatch) Append(write TimestampWrite) bool {
	if len(b.Writes) == cap(b.Writes) || b.inFlight.Load() {
		return false
	}
	b.Writes = append(b.Writes, write)
	return true
}

func (b *TimestampWriteBatch) Reset() bool {
	if b.inFlight.Load() {
		return false
	}
	b.Writes = b.Writes[:0]
	return true
}

func (i *Index) Submit(batch *TimestampWriteBatch) error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if batch == nil || batch.done == nil {
		return ErrUninitializedBatch
	}
	if i.closed.Load() {
		return ErrClosed
	}
	if !batch.inFlight.CompareAndSwap(false, true) {
		return ErrBatchInFlight
	}
	select {
	case i.writes <- batch:
	case <-i.stop:
		batch.inFlight.Store(false)
		return ErrClosed
	}
	err := <-batch.done
	batch.inFlight.Store(false)
	return err
}

func (i *Index) writerLoop() {
	defer i.workers.Done()
	stopping := false
	outstanding := 0
	for {
		if stopping {
			for {
				select {
				case batch := <-i.writes:
					batch.done <- ErrClosed
				default:
					goto drained
				}
			}
		}
	drained:
		if stopping && outstanding == 0 {
			close(i.compactions)
			return
		}

		select {
		case batch := <-i.writes:
			if stopping {
				batch.done <- ErrClosed
				continue
			}
			rotations, err := i.applyBatch(batch.Writes)
			outstanding += rotations
			batch.done <- err
		case response := <-i.barriers:
			var result barrierResult
			if i.wal != nil {
				result.walOffset, result.err = i.wal.offset()
			}
			response <- result
		case result := <-i.compactDone:
			if result.reclaimed != nil {
				result.block.spare = result.reclaimed
			}
			outstanding--
			i.pending.Add(-1)
		case <-i.stop:
			stopping = true
		}
	}
}

func (i *Index) applyBatch(writes []TimestampWrite) (int, error) {
	if len(writes) == 0 {
		return 0, nil
	}
	directory := i.directory.Load()
	i.writerTouched = i.writerTouched[:0]
	for _, write := range writes {
		if write.Op != WriteAdd && write.Op != WriteDelete {
			i.clearWriterNeeds()
			return 0, ErrInvalidWrite
		}
		marker, err := EncodeTimemarkerChecked(write.Timestamp, i.offset)
		if err != nil {
			i.clearWriterNeeds()
			return 0, err
		}
		blockIndex := directory.blockFor(uint64(marker))
		if i.writerNeeds[blockIndex] == 0 {
			i.writerTouched = append(i.writerTouched, blockIndex)
		}
		i.writerNeeds[blockIndex]++
	}

	for _, blockIndex := range i.writerTouched {
		needed := i.writerNeeds[blockIndex]
		if needed > uint32(i.config.DeltaCapacity) {
			i.clearWriterNeeds()
			return 0, ErrBatchTooLarge
		}
		block := directory.entries[blockIndex].block
		view := block.view.Load()
		if needed > view.Active.remaining() && (view.Frozen != nil || block.spare == nil) {
			i.clearWriterNeeds()
			return 0, ErrBackpressure
		}
	}
	if i.wal != nil {
		if err := i.wal.appendBatch(writes, i.config.Durability); err != nil {
			i.clearWriterNeeds()
			return 0, err
		}
	}

	rotations := 0
	for _, blockIndex := range i.writerTouched {
		block := directory.entries[blockIndex].block
		view := block.view.Load()
		if i.writerNeeds[blockIndex] > view.Active.remaining() {
			i.rotateBlock(block, view)
			rotations++
		}
	}

	for _, write := range writes {
		marker, _ := EncodeTimemarkerChecked(write.Timestamp, i.offset)
		block := directory.entries[directory.blockFor(uint64(marker))].block
		active := block.view.Load().Active
		if write.Op == WriteAdd {
			active.appendAdd(marker, write.ImageID)
		} else {
			active.appendDelete(write.ImageID)
		}
	}
	i.clearWriterNeeds()
	return rotations, nil
}

func (i *Index) clearWriterNeeds() {
	for _, blockIndex := range i.writerTouched {
		i.writerNeeds[blockIndex] = 0
	}
	i.writerTouched = i.writerTouched[:0]
}

func (i *Index) rotateBlock(block *TimestampBlock, expected *TimestampBlockView) {
	newActive := block.spare
	block.spare = nil
	view := &TimestampBlockView{
		Base:       expected.Base,
		Frozen:     expected.Active,
		Active:     newActive,
		Generation: expected.Generation + 1,
	}
	block.view.Store(view)
	i.pending.Add(1)
	i.compactions <- compactionTask{block: block, expected: view, frozen: expected.Active}
}
