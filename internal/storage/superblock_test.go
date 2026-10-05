package storage

import (
	"bytes"
	"testing"
)

func TestSuperblockRoundTripAndSelection(t *testing.T) {
	const magic uint32 = 0x52485342
	const version uint16 = 2
	slots := [SuperblockSlots][]byte{
		make([]byte, SuperblockSlotBytes),
		make([]byte, SuperblockSlotBytes),
	}
	if _, _, ok := DecodeSuperblock(slots[0], magic, version); ok {
		t.Fatal("empty slot decoded")
	}

	payloadA := []byte{1, 2, 3}
	if err := EncodeSuperblock(slots[0], SuperblockHeader{Magic: magic, Version: version, Generation: 5}, payloadA); err != nil {
		t.Fatalf("EncodeSuperblock: %v", err)
	}
	payloadB := []byte{4, 5, 6, 7}
	if err := EncodeSuperblock(slots[1], SuperblockHeader{Magic: magic, Version: version, Generation: 9}, payloadB); err != nil {
		t.Fatalf("EncodeSuperblock: %v", err)
	}

	slot, header, payload, ok := SelectSuperblock(slots, magic, version)
	if !ok || slot != 1 || header.Generation != 9 || !bytes.Equal(payload, payloadB) {
		t.Fatalf("SelectSuperblock = slot %d gen %d payload %v (ok %v)", slot, header.Generation, payload, ok)
	}

	// A corrupt newer slot falls back to the older valid one.
	slots[1][24] ^= 0xff
	slot, header, payload, ok = SelectSuperblock(slots, magic, version)
	if !ok || slot != 0 || header.Generation != 5 || !bytes.Equal(payload, payloadA) {
		t.Fatalf("SelectSuperblock(corrupt newer) = slot %d gen %d (ok %v)", slot, header.Generation, ok)
	}

	// Wrong version and wrong magic are rejected, not misdecoded.
	if _, _, ok := DecodeSuperblock(slots[0], magic, version+1); ok {
		t.Fatal("wrong version decoded")
	}
	if _, _, ok := DecodeSuperblock(slots[0], magic+1, version); ok {
		t.Fatal("wrong magic decoded")
	}
	// A payload that cannot fit the envelope is rejected.
	if err := EncodeSuperblock(slots[0], SuperblockHeader{Magic: magic, Version: version}, make([]byte, SuperblockSlotBytes)); err == nil {
		t.Fatal("oversized payload encoded")
	}
}
