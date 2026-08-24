package geoindex

import (
	"errors"
	"testing"

	"github.com/golang/geo/s2"
)

func TestS2KeyRoundTripAndLevelValidation(t *testing.T) {
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(GeoS2Level)
	key, err := S2KeyFromCellID(uint64(cell))
	if err != nil {
		t.Fatal(err)
	}
	if DecodeS2Key(key) != uint64(cell) {
		t.Fatalf("round trip: got %x want %x", DecodeS2Key(key), uint64(cell))
	}
	decoded, err := CellIDFromS2Key(key)
	if err != nil || decoded != cell {
		t.Fatalf("decoded %v, %v", decoded, err)
	}
	_, err = S2KeyFromCellID(uint64(cell.Parent(15)))
	if !errors.Is(err, ErrInvalidS2Level) {
		t.Fatalf("wrong level error: %v", err)
	}
	if _, err = CellIDFromS2Key(S2Key(1) << 50); !errors.Is(err, ErrInvalidS2Cell) {
		t.Fatalf("high-bit key error: %v", err)
	}
}

func TestRangesFromCellIDsNormalizesAdjacentCells(t *testing.T) {
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(GeoS2Level)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell.Next()), uint64(cell), uint64(cell.Prev())})
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != (Range{Start: EncodeS2Key(uint64(cell.Prev())), End: EncodeS2Key(uint64(cell.Next()))}) {
		t.Fatalf("ranges = %#v", ranges)
	}
}

func TestRangesFromCoarseCell(t *testing.T) {
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(12)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell)})
	if err != nil {
		t.Fatal(err)
	}
	want := Range{
		Start: EncodeS2Key(uint64(cell.ChildBeginAtLevel(GeoS2Level))),
		End:   EncodeS2Key(uint64(cell.ChildEndAtLevel(GeoS2Level).Prev())),
	}
	if len(ranges) != 1 || ranges[0] != want {
		t.Fatalf("ranges = %#v, want %#v", ranges, want)
	}
}
