package geoidx

import (
	"errors"
	"math"

	"github.com/golang/geo/s2"

	common "github.com/evanoberholster/imageIdx/common"
)

var (
	// ErrClosed reports an operation on a closed index.
	ErrClosed = errors.New("geoidx: index is closed")
	// ErrInvalidID reports the reserved zero ImageID.
	ErrInvalidID = errors.New("geoidx: invalid image id")
	// ErrInvalidKey reports a key that is not a valid S2Level cell key.
	ErrInvalidKey = errors.New("geoidx: invalid s2 key")
	// ErrInvalidS2Cell reports an invalid or zero S2 cell ID.
	ErrInvalidS2Cell = errors.New("geoidx: invalid s2 cell")
	// ErrInvalidS2Level reports an S2 cell at a level other than S2Level where
	// only S2Level is accepted.
	ErrInvalidS2Level = errors.New("geoidx: invalid s2 level")
	// ErrInvalidCoordinate reports a non-finite or out-of-range coordinate.
	ErrInvalidCoordinate = errors.New("geoidx: invalid coordinate")
	// ErrTooManyCells reports a cover that would exceed its cell budget.
	ErrTooManyCells = errors.New("geoidx: cover exceeds cell budget")
	// ErrInvalidConfig reports an unusable configuration.
	ErrInvalidConfig = errors.New("geoidx: invalid configuration")
	// ErrCorrupt reports a corrupt base file, superblock, or manifest.
	ErrCorrupt = errors.New("geoidx: corrupt index")
)

const (
	// S2Level is the fixed S2 cell level of every key in the index. See the
	// package doc for the precision rationale.
	S2Level = 19
	// keyShift drops the trailing zero bits of a level-19 cell ID.
	keyShift = 60 - 2*S2Level // 22

	// earthRadiusMeters is the mean Earth radius used for great-circle distance.
	earthRadiusMeters = 6_371_008.8
)

// S2Key is a fixed-level S2 cell key: uint64(cellID) >> keyShift. Keys are
// contiguous, so the descendants of any cell form a contiguous key interval.
type S2Key uint64

// EncodeKey converts a full S2 cell ID to its key. It does not validate level;
// use KeyFromCellID for validated input.
func EncodeKey(cellID uint64) S2Key { return S2Key(cellID >> keyShift) }

// DecodeKey expands a key back to a full cell ID. It does not validate level;
// use CellIDFromKey for validated input.
func DecodeKey(key S2Key) uint64 { return uint64(key) << keyShift }

// KeyFromCellID converts a level-S2Level cell ID to its key, rejecting invalid
// cells and wrong levels.
func KeyFromCellID(cellID uint64) (S2Key, error) {
	cell := s2.CellID(cellID)
	if !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != S2Level {
		return 0, ErrInvalidS2Level
	}
	return EncodeKey(cellID), nil
}

// CellIDFromKey converts a key back to its cell ID, verifying the round trip
// and level. It guards persisted and query inputs against truncated keys.
func CellIDFromKey(key S2Key) (s2.CellID, error) {
	cell := s2.CellID(DecodeKey(key))
	if uint64(cell)>>keyShift != uint64(key) || !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != S2Level {
		return 0, ErrInvalidS2Level
	}
	return cell, nil
}

// cellFromLatLng converts a coordinate to its level-S2Level cell, rejecting
// non-finite and out-of-range input.
func cellFromLatLng(latitude, longitude float64) (s2.CellID, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) ||
		math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return 0, ErrInvalidCoordinate
	}
	return s2.CellIDFromLatLng(s2.LatLngFromDegrees(latitude, longitude)).Parent(S2Level), nil
}

// CellIDFromLatLng converts a coordinate to its level-S2Level cell ID.
func CellIDFromLatLng(latitude, longitude float64) (uint64, error) {
	cell, err := cellFromLatLng(latitude, longitude)
	if err != nil {
		return 0, err
	}
	return uint64(cell), nil
}

// KeyFromLatLng converts a coordinate to its S2 key.
func KeyFromLatLng(latitude, longitude float64) (S2Key, error) {
	cell, err := cellFromLatLng(latitude, longitude)
	if err != nil {
		return 0, err
	}
	return EncodeKey(uint64(cell)), nil
}

// DistanceMeters returns the exact great-circle distance between two
// coordinates in meters.
func DistanceMeters(a, b s2.LatLng) float64 {
	return a.Distance(b).Radians() * earthRadiusMeters
}

// DistanceBetween returns the exact great-circle distance in meters between two
// latitude/longitude coordinates.
func DistanceBetween(lat1, lng1, lat2, lng2 float64) float64 {
	return DistanceMeters(s2.LatLngFromDegrees(lat1, lng1), s2.LatLngFromDegrees(lat2, lng2))
}

// CoordinateLookup returns the canonical coordinate for an ImageID. The
// higher-level search ranker supplies it; geoidx stores no coordinates.
type CoordinateLookup func(id common.ImageID) (latitude, longitude float64, ok bool)

// keyDistanceMeters returns the great-circle distance from the center of key's
// cell to target. Coordinates are not stored, so this is the finest distance the
// index can report.
func keyDistanceMeters(key S2Key, target s2.LatLng) float64 {
	cell := s2.CellID(DecodeKey(key))
	return DistanceMeters(cell.LatLng(), target)
}

// Entry is one stored image location.
type Entry struct {
	Key     S2Key
	ImageID common.ImageID
}
