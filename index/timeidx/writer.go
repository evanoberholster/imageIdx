package timeidx

import "sync/atomic"

type barrierResult struct {
	walOffset int64
	err       error
}

// rotateRequest asks the single-owner writerLoop to roll the WAL to a fresh
// epoch. fromOffset is the checkpoint barrier offset whose complete tail
// must be carried into the new epoch; epoch names the new WAL file. The
// writerLoop is the only goroutine that appends to the WAL, so performing
// the tail copy and handle swap there keeps rotation atomic with respect to
// concurrent write batches: batches handled before the swap land in the
// copied tail, batches after it append to the new epoch.
type rotateRequest struct {
	oldEpoch   uint64
	epoch      uint64
	fromOffset int64
	result     chan rotateResult
}

type rotateResult struct {
	err error
}

type mutationKind uint8

const (
	mutationInsert mutationKind = iota + 1
	mutationDelete
	mutationMove
)

// Mutation is one logical timestamp insertion, deletion, or move. Constructors
// keep the discriminator private so invalid combinations cannot be built.
type Mutation struct {
	imageID ImageID
	from    uint64
	to      uint64
	kind    mutationKind
}

// Insert creates a mutation for an image absent from the timestamp index.
func Insert(imageID ImageID, timestamp uint64) Mutation {
	return Mutation{imageID: imageID, to: timestamp, kind: mutationInsert}
}

// Delete creates a mutation that removes an image at its canonical timestamp.
func Delete(imageID ImageID, timestamp uint64) Mutation {
	return Mutation{imageID: imageID, from: timestamp, kind: mutationDelete}
}

// Move creates one logical mutation that removes from and inserts at to.
func Move(imageID ImageID, from, to uint64) Mutation {
	return Mutation{imageID: imageID, from: from, to: to, kind: mutationMove}
}

type writeOp uint8

const (
	writeAdd writeOp = iota + 1
	writeDelete
)

type write struct {
	timestamp uint64
	imageID   ImageID
	op        writeOp
}

// Batch is reusable writer-owned storage for logical mutations. It is not
// safe for concurrent use and cannot be changed while Apply is in flight.
type Batch struct {
	writes    []write
	mutations int
	capacity  int

	done     chan error
	inFlight atomic.Bool
}

// NewBatch allocates capacity for logical mutations. Moves expand to two
// private writes without growing the batch.
func NewBatch(capacity int) *Batch {
	if capacity < 0 {
		capacity = 0
	}
	return &Batch{
		writes:   make([]write, 0, capacity*2),
		capacity: capacity,
		done:     make(chan error, 1),
	}
}

// Append adds one logical mutation without growing the batch. It returns false
// for an invalid mutation, a full batch, or a batch currently in flight.
func (b *Batch) Append(mutation Mutation) bool {
	if b == nil || b.mutations == b.capacity || b.inFlight.Load() {
		return false
	}
	switch mutation.kind {
	case mutationInsert:
		b.writes = append(b.writes, write{timestamp: mutation.to, imageID: mutation.imageID, op: writeAdd})
	case mutationDelete:
		b.writes = append(b.writes, write{timestamp: mutation.from, imageID: mutation.imageID, op: writeDelete})
	case mutationMove:
		b.writes = append(b.writes,
			write{timestamp: mutation.from, imageID: mutation.imageID, op: writeDelete},
			write{timestamp: mutation.to, imageID: mutation.imageID, op: writeAdd},
		)
	default:
		return false
	}
	b.mutations++
	return true
}

// Reset clears accepted mutations while retaining storage. It returns false
// while the batch is in flight.
func (b *Batch) Reset() bool {
	if b == nil || b.inFlight.Load() {
		return false
	}
	b.writes = b.writes[:0]
	b.mutations = 0
	return true
}

// Len reports the number of logical mutations, counting a move once.
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return b.mutations
}

// Apply sends one reusable batch through the single-owner writer and waits for
// its atomic acceptance or rejection.
func (i *Index) Apply(batch *Batch) error {
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
				case req := <-i.rotate:
					req.result <- rotateResult{err: ErrClosed}
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
			rotations, err := i.applyBatch(batch.writes)
			outstanding += rotations
			batch.done <- err
		case response := <-i.barriers:
			var result barrierResult
			if i.persistence != nil && i.persistence.wal != nil {
				result.walOffset, result.err = i.persistence.wal.Offset()
			}
			response <- result
		case req := <-i.rotate:
			req.result <- i.rotateWAL(req)
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

func (i *Index) applyBatch(writes []write) (int, error) {
	if len(writes) == 0 {
		return 0, nil
	}
	directory := i.directory.Load()
	i.writerTouched = i.writerTouched[:0]
	for _, write := range writes {
		if write.op != writeAdd && write.op != writeDelete {
			i.clearWriterNeeds()
			return 0, ErrInvalidMutation
		}
		marker, err := encodeTimemarkerChecked(write.timestamp, i.offset)
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
		if needed > view.active.remaining() && (view.frozen != nil || block.spare == nil) {
			i.clearWriterNeeds()
			return 0, ErrBackpressure
		}
	}
	if i.persistence != nil && i.persistence.wal != nil {
		if err := i.persistence.wal.appendBatch(writes, i.persistence.durability); err != nil {
			i.clearWriterNeeds()
			return 0, err
		}
	}

	rotations := 0
	for _, blockIndex := range i.writerTouched {
		block := directory.entries[blockIndex].block
		view := block.view.Load()
		if i.writerNeeds[blockIndex] > view.active.remaining() {
			i.rotateBlock(block, view)
			rotations++
		}
	}

	for _, write := range writes {
		marker, _ := encodeTimemarkerChecked(write.timestamp, i.offset)
		block := directory.entries[directory.blockFor(uint64(marker))].block
		active := block.view.Load().active
		if write.op == writeAdd {
			active.appendAdd(marker, write.imageID)
		} else {
			active.appendDelete(write.imageID)
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

func (i *Index) rotateBlock(block *block, expected *blockView) {
	newActive := block.spare
	block.spare = nil
	view := &blockView{
		base:       expected.base,
		frozen:     expected.active,
		active:     newActive,
		generation: expected.generation + 1,
	}
	block.view.Store(view)
	i.pending.Add(1)
	i.compactions <- compactionTask{block: block, expected: view, frozen: expected.active}
}
