package timeidx

import (
	"math"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

const testNamespace common.Namespace = 9

func TestTimemarkerEncoding(t *testing.T) {
	const offset = uint64(1_700_000_000_000_000_000)
	const lowBits = uint64(0x00ab_cdef)
	timestamp := offset + (uint64(42) << TimestampShift) + lowBits
	marker, err := encodeTimemarkerChecked(timestamp, offset)
	if err != nil {
		t.Fatal(err)
	}
	if marker != 42 {
		t.Fatalf("marker = %d, want 42", marker)
	}
	if decoded := decodeTimemarker(marker, offset); decoded != timestamp-lowBits {
		t.Fatalf("decoded = %d, want %d", decoded, timestamp-lowBits)
	}
}

func TestTimemarkerRangeValidation(t *testing.T) {
	const offset = uint64(100)
	if _, err := encodeTimemarkerChecked(offset-1, offset); err != ErrInvalidRange {
		t.Fatalf("underflow error = %v, want %v", err, ErrInvalidRange)
	}
	last := offset + (uint64(math.MaxUint32) << TimestampShift) + (1<<TimestampShift - 1)
	marker, err := encodeTimemarkerChecked(last, offset)
	if err != nil || marker != timemarker(math.MaxUint32) {
		t.Fatalf("last representable marker = %d, err = %v", marker, err)
	}
	if _, err := encodeTimemarkerChecked(last+1, offset); err != ErrInvalidRange {
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

func TestMetadataCopiesStableIdentity(t *testing.T) {
	index := mustNewIndex(t, nil, 42, Config{})
	index.namespace = testNamespace
	index.flags = 3
	metadata := index.Metadata()
	if metadata.Namespace != testNamespace || metadata.Flags != 3 || metadata.Offset != 42 || metadata.Generation != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestSpecNamespaceDef(t *testing.T) {
	def := Spec{Namespace: testNamespace, Name: "capture-time"}.NamespaceDef()
	if def.Namespace != testNamespace || def.Name != "capture-time" || def.Kind != common.KindTimestamp {
		t.Fatalf("NamespaceDef() = %+v", def)
	}
}
