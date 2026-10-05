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

// TestSuperblockRejectsTruncatedAndCorruptPayload checks that decoding requires
// a complete slot and that a payload CRC failure is rejected rather than read.
func TestSuperblockRejectsTruncatedAndCorruptPayload(t *testing.T) {
	const magic uint32 = 0x52485342
	const version uint16 = 2
	full := make([]byte, SuperblockSlotBytes)
	if err := EncodeSuperblock(full, SuperblockHeader{Magic: magic, Version: version, Generation: 1}, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("EncodeSuperblock: %v", err)
	}
	if _, _, ok := DecodeSuperblock(full, magic, version); !ok {
		t.Fatal("valid slot rejected")
	}
	if _, _, ok := DecodeSuperblock(full[:len(full)-1], magic, version); ok {
		t.Fatal("truncated slot accepted")
	}
	oversized := append(append([]byte(nil), full...), 0)
	if _, _, ok := DecodeSuperblock(oversized, magic, version); ok {
		t.Fatal("oversized slot accepted")
	}
	corrupt := append([]byte(nil), full...)
	corrupt[superblockHeaderBytes] ^= 0xff
	if _, _, ok := DecodeSuperblock(corrupt, magic, version); ok {
		t.Fatal("corrupt payload accepted")
	}
}
