package geoindex

import (
	"math"

	"github.com/golang/geo/s2"
)

func EncodeS2Key(cellID uint64) S2Key { return S2Key(cellID >> 28) }
func DecodeS2Key(key S2Key) uint64    { return uint64(key) << 28 }

func S2KeyFromCellID(cellID uint64) (S2Key, error) {
	cell := s2.CellID(cellID)
	if !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != GeoS2Level {
		return 0, ErrInvalidS2Level
	}
	return EncodeS2Key(cellID), nil
}

func CellIDFromS2Key(key S2Key) (s2.CellID, error) {
	if EncodeS2Key(DecodeS2Key(key)) != key {
		return 0, ErrInvalidS2Cell
	}
	cell := s2.CellID(DecodeS2Key(key))
	if !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != GeoS2Level {
		return 0, ErrInvalidS2Level
	}
	return cell, nil
}

func KeyFromLatLng(latitude, longitude float64) (S2Key, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) || math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return 0, ErrInvalidCoordinate
	}
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(latitude, longitude)).Parent(GeoS2Level)
	return EncodeS2Key(uint64(cell)), nil
}
