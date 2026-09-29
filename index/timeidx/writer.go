package timeidx

import (
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
)

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
	imageID common.ValueID
	from    uint64
	to      uint64
	kind    mutationKind
}

// Insert creates a mutation for an image absent from the timestamp index.
func Insert(imageID common.ValueID, timestamp uint64) Mutation {
	return Mutation{imageID: imageID, to: timestamp, kind: mutationInsert}
}

// Delete creates a mutation that removes an image at its canonical timestamp.
func Delete(imageID common.ValueID, timestamp uint64) Mutation {
	return Mutation{imageID: imageID, from: timestamp, kind: mutationDelete}
}

// Move creates one logical mutation that removes from and inserts at to.
func Move(imageID common.ValueID, from, to uint64) Mutation {
	return Mutation{imageID: imageID, from: from, to: to, kind: mutationMove}
}

type writeOp uint8

const (
	writeAdd writeOp = iota + 1
	writeDelete
)

type write struct {
	timestamp uint64
	imageID   common.ValueID
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

// maxBatchCapacity caps Batch capacity so the doubled write backing
// (moves expand to two private writes) stays bounded and capacity*2 can
// never overflow. Larger logical batches must be split by the caller;
// Append reports full past the cap exactly as for a filled batch.
const maxBatchCapacity = 1 << 20

// NewBatch allocates capacity for logical mutations. Moves expand to two
// private writes without growing the batch. Note the WAL frame count is
// uint16, so even a capped batch fails late with ErrBatchTooLarge past
// 65535 writes; size batches accordingly (moves count double).
func NewBatch(capacity int) *Batch {
	if capacity < 0 {
		capacity = 0
	}
	if capacity > maxBatchCapacity {
		capacity = maxBatchCapacity
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
// its atomic acceptance or rejection. Acceptance is atomic: validation and
// the single WAL append either happen for the whole batch or fail it whole.
// Visibility is per write, not per batch: deltas append in place, so a
// concurrent reader may observe a batch prefix before the writer finishes;
// the seqlock retry only guarantees no torn reads. A latched WAL failure
// fails fast with ErrRecoveryRequired until close and reopen.
func (i *Index) Apply(batch *Batch) error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if batch == nil || batch.done == nil {
		return ErrUninitializedBatch
	}
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.failedErr(); err != nil {
		return err
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

// drainStopped rejects everything still queued after stop with ErrClosed.
// It runs only on the writer loop; the channel selects never block because
// every case either receives or falls through to default.
func (i *Index) drainStopped() {
	for {
		select {
		case batch := <-i.writes:
			batch.done <- ErrClosed
		case req := <-i.rotate:
			req.result <- rotateResult{err: ErrClosed}
		default:
			return
		}
	}
}

func (i *Index) writerLoop() {
	defer i.workers.Done()
	stopping := false
	outstanding := 0
	for {
		if stopping {
			i.drainStopped()
		}
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
			outstanding += i.applyGroup(i.collectWrites(batch))
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

// collectWrites gathers the leader batch plus every batch already queued
// behind it. Bursts commit as one group sharing one WAL sync; an idle index
// commits a group of one with no added latency.
func (i *Index) collectWrites(first *Batch) []*Batch {
	i.writeGroup = append(i.writeGroup[:0], first)
	for {
		select {
		case batch := <-i.writes:
			i.writeGroup = append(i.writeGroup, batch)
		default:
			return i.writeGroup
		}
	}
}

// applyGroup commits one coalesced writer-loop group. Each batch keeps its
// own validation, rotation, and result, exactly as when applied alone; the
// group shares one WAL sync under DurabilitySync, so N queued batches pay
// one fsync instead of N. A sync failure converts the group's successful
// results into the latched recovery error.
func (i *Index) applyGroup(group []*Batch) int {
	i.writeResults = i.writeResults[:0]
	rotations := 0
	wrote := false
	for _, batch := range group {
		rotated, err := i.applyBatch(batch.writes)
		rotations += rotated
		i.writeResults = append(i.writeResults, err)
		if err == nil && len(batch.writes) != 0 {
			wrote = true
		}
	}
	if wrote && i.persistence != nil && i.persistence.wal != nil && i.persistence.durability == DurabilitySync {
		if err := i.persistence.wal.Sync(); err != nil {
			syncErr := i.fail(err)
			for n := range i.writeResults {
				if i.writeResults[n] == nil {
					i.writeResults[n] = syncErr
				}
			}
		}
	}
	for n, batch := range group {
		batch.done <- i.writeResults[n]
	}
	return rotations
}

// applyBatch validates and applies one batch's writes. It never fsyncs:
// the writer loop coalesces a group and syncs once at the end, so a batch
// applied alone is durable only after applyGroup's sync.
func (i *Index) applyBatch(writes []write) (int, error) {
	if len(writes) == 0 {
		return 0, nil
	}
	directory := i.directory.Load()
	i.writerTouched = i.writerTouched[:0]
	i.writerMarks = i.writerMarks[:0]
	i.writerBlocks = i.writerBlocks[:0]
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
		i.writerMarks = append(i.writerMarks, uint64(marker))
		i.writerBlocks = append(i.writerBlocks, blockIndex)
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
		if err := i.persistence.wal.appendBatch(writes, false); err != nil {
			i.clearWriterNeeds()
			return 0, i.fail(err)
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

	for index, write := range writes {
		block := directory.entries[i.writerBlocks[index]].block
		active := block.view.Load().active
		if write.op == writeAdd {
			active.appendAdd(timemarker(i.writerMarks[index]), write.imageID)
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
	i.writerMarks = i.writerMarks[:0]
	i.writerBlocks = i.writerBlocks[:0]
}

// rotateBlock freezes the active delta and publishes a fresh one from the
// block's spare. It runs only on the writer loop. A nil spare with no
// frozen layer is unreachable: the spare starts populated, every rotation
// consumes it while publishing a frozen layer, and the backpressure guard
// in applyBatch refuses further rotations until compaction reclaims one.
// So a rotation always lands with somewhere to write.
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
