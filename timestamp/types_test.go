package timestamp

import (
	"math"
	"testing"
)

func TestTimemarkerEncoding(t *testing.T) {
	const offset = uint64(1_700_000_000_000_000_000)
	const lowBits = uint64(0x00ab_cdef)
	timestamp := offset + (uint64(42) << TimestampShift) + lowBits
	marker, err := EncodeTimemarkerChecked(timestamp, offset)
	if err != nil {
		t.Fatal(err)
	}
	if marker != 42 {
		t.Fatalf("marker = %d, want 42", marker)
	}
	if decoded := DecodeTimemarker(marker, offset); decoded != timestamp-lowBits {
		t.Fatalf("decoded = %d, want %d", decoded, timestamp-lowBits)
	}
}

func TestTimemarkerRangeValidation(t *testing.T) {
	const offset = uint64(100)
	if _, err := EncodeTimemarkerChecked(offset-1, offset); err != ErrInvalidRange {
		t.Fatalf("underflow error = %v, want %v", err, ErrInvalidRange)
	}
	last := offset + (uint64(math.MaxUint32) << TimestampShift) + (1<<TimestampShift - 1)
	marker, err := EncodeTimemarkerChecked(last, offset)
	if err != nil || marker != Timemarker(math.MaxUint32) {
		t.Fatalf("last representable marker = %d, err = %v", marker, err)
	}
	if _, err := EncodeTimemarkerChecked(last+1, offset); err != ErrInvalidRange {
		t.Fatalf("overflow error = %v, want %v", err, ErrInvalidRange)
	}
}

func TestMarkerRangeUsesConservativeBoundaries(t *testing.T) {
	const offset = uint64(1_000)
	const quantum = uint64(1) << TimestampShift
	lo, hi, ok := markerRange(offset+quantum+17, offset+2*quantum+23, offset)
	if !ok || lo != 1 || hi != 3 {
		t.Fatalf("markerRange = (%d, %d, %v), want (1, 3, true)", lo, hi, ok)
	}
}

func TestHeaderValidation(t *testing.T) {
	header := TimestampIndexHeader{
		Magic:      TimestampIndexMagic,
		Version:    TimestampIndexVersion,
		Count:      1,
		StartTime:  10,
		EndTime:    11,
		Shift:      TimestampShift,
		Generation: 1,
	}
	if err := header.Validate(); err != nil {
		t.Fatal(err)
	}
	header.Shift = TimestampShift - 1
	if err := header.Validate(); err == nil {
		t.Fatal("invalid shift was accepted")
	}
}
