package simhash

import (
	"encoding/binary"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// The update journal is a redo log for in-place point updates. It is
// batch-oriented: one WAL frame carries one logical update transaction with a
// base generation and a new generation, so recovery never exposes half a batch.
//
// Protocol (writer serialized by the store mutex):
//
//  1. validate the whole batch
//  2. encode one complete batch frame (base generation, new generation,
//     fixed-width updates, CRC)
//  3. append the frame; fsync it under DurabilitySync
//  4. group affected sequence blocks and apply in place (seq odd -> updates
//     -> seq even per block)
//  5. advance and publish the generation once
//  6. Checkpoint flushes canonical data, shard metadata, then the manifest,
//     and only then truncates the journal
//
// A truncated frame or a frame whose CRC fails is corruption or an interrupted
// tail; the tail is repaired away. Replay is idempotent: each update rewrites
// the full tuple. The manifest is the durable floor; a validated chain of WAL
// batches may advance the generation beyond it until the next checkpoint.
const (
	journalMagic   uint32 = 0x534A524E // "SJRN"
	journalVersion uint16 = 2          // batch frame
	// journalHeaderBytes is the transport header: magic(4) version(2)
	// reserved(2) payloadLen(4) reserved(4).
	journalHeaderBytes = 16

	// Payload layout: baseGeneration(8) newGeneration(8) count(4), then
	// count * update(49), then CRC32C(4).
	batchFixedBytes  = 8 + 8 + 4
	batchUpdateBytes = 8 + 8 + 32 + 1
	batchCRCBytes    = 4

	// maxBatchRecords bounds one batch frame so a corrupt length cannot cause
	// an unbounded recovery allocation.
	maxBatchRecords      = 4096
	maxBatchPayloadBytes = batchFixedBytes + maxBatchRecords*batchUpdateBytes + batchCRCBytes

	journalFilename = "updates.wal"
)

type journal struct {
	wal *storage.WAL
}

func openJournal(path string) (*journal, error) {
	w, err := storage.OpenWAL(path, journalHeaderBytes+2*batchUpdateBytes)
	if err != nil {
		return nil, err
	}
	return &journal{wal: w}, nil
}

func (j *journal) close() error {
	if j == nil || j.wal == nil {
		return nil
	}
	return j.wal.Close()
}

// reset truncates the journal to empty. The caller must have made the canonical
// columns and authoritative metadata durable first.
func (j *journal) reset() error { return j.wal.RepairTail(0) }

// appendBatch writes one redo batch frame. It allocates nothing in steady state.
func (j *journal) appendBatch(base, next uint64, updates []Update, sync bool) error {
	payloadLen := batchFixedBytes + len(updates)*batchUpdateBytes + batchCRCBytes
	buf := j.wal.Scratch(journalHeaderBytes + payloadLen)
	header := buf[:journalHeaderBytes]
	payload := buf[journalHeaderBytes:]
	binary.LittleEndian.PutUint32(header[0:4], journalMagic)
	binary.LittleEndian.PutUint16(header[4:6], journalVersion)
	binary.LittleEndian.PutUint32(header[8:12], uint32(payloadLen))
	binary.LittleEndian.PutUint64(payload[0:8], base)
	binary.LittleEndian.PutUint64(payload[8:16], next)
	binary.LittleEndian.PutUint32(payload[16:20], uint32(len(updates)))
	off := batchFixedBytes
	for i := range updates {
		binary.LittleEndian.PutUint64(payload[off:off+8], uint64(updates[i].ImageID))
		off += 8
		binary.LittleEndian.PutUint64(payload[off:off+8], updates[i].Record.PHash)
		off += 8
		copy(payload[off:off+32], updates[i].Record.PDQ[:])
		off += 32
		payload[off] = updates[i].Record.Quality
		off++
	}
	binary.LittleEndian.PutUint32(payload[off:off+4], storage.Checksum(payload[:off]))
	return j.wal.AppendFrame(header, payload, sync)
}

// batchCount validates a batch payload (length, count, CRC) and returns the
// record count.
func batchCount(payload []byte) (int, bool) {
	if len(payload) < batchFixedBytes+batchUpdateBytes+batchCRCBytes {
		return 0, false
	}
	count := int(binary.LittleEndian.Uint32(payload[16:20]))
	if count <= 0 || count > maxBatchRecords {
		return 0, false
	}
	want := batchFixedBytes + count*batchUpdateBytes + batchCRCBytes
	if want != len(payload) {
		return 0, false
	}
	if binary.LittleEndian.Uint32(payload[want-4:want]) != storage.Checksum(payload[:want-4]) {
		return 0, false
	}
	return count, true
}

func batchGenerations(payload []byte) (base, next uint64) {
	return binary.LittleEndian.Uint64(payload[0:8]), binary.LittleEndian.Uint64(payload[8:16])
}

// forEachUpdate iterates the fixed-width updates in a validated payload.
func forEachUpdate(payload []byte, fn func(imageID, phash uint64, pdq *PDQHash, quality uint8)) {
	count := int(binary.LittleEndian.Uint32(payload[16:20]))
	off := batchFixedBytes
	for i := 0; i < count; i++ {
		id := binary.LittleEndian.Uint64(payload[off : off+8])
		off += 8
		ph := binary.LittleEndian.Uint64(payload[off : off+8])
		off += 8
		var pdq PDQHash
		copy(pdq[:], payload[off:off+32])
		off += 32
		q := payload[off]
		off++
		fn(id, ph, &pdq, q)
	}
}

func journalPayloadLen(header []byte) (uint64, error) {
	if len(header) < journalHeaderBytes {
		return 0, ErrCorrupt
	}
	return uint64(binary.LittleEndian.Uint32(header[8:12])), nil
}

// replayJournal visits every complete, well-formed batch frame in order and
// returns the exclusive end offset of the last complete frame.
func replayJournal(path string, visit func(base, next uint64, payload []byte) error) (int64, error) {
	return storage.ReplayEnd(path, 0, storage.FrameSpec{
		HeaderSize: journalHeaderBytes,
		Magic:      journalMagic,
		Version:    journalVersion,
		Corrupt:    ErrCorrupt,
		MaxPayload: maxBatchPayloadBytes,
	}, journalPayloadLen, func(header, payload []byte) error {
		if _, ok := batchCount(payload); !ok {
			return ErrCorrupt
		}
		base, next := batchGenerations(payload)
		return visit(base, next, payload)
	})
}
