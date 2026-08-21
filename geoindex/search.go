package geoindex

import (
	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/golang/geo/s2"
)

func (g *GeoIndex) CoverRegion(region s2.Region) ([]S2KeyRange, error) {
	if region == nil {
		return nil, ErrInvalidCoordinate
	}
	coverer := &s2.RegionCoverer{MinLevel: 0, MaxLevel: GeoS2Level, LevelMod: 1, MaxCells: g.config.MaxCoverCells}
	return RangesFromCellUnion(coverer.Covering(region))
}

func (g *GeoIndex) SearchRegion(dst []ImageID, region s2.Region, lookup CoordinateLookup) []ImageID {
	ranges, err := g.CoverRegion(region)
	if err != nil {
		return dst
	}
	start := len(dst)
	dst = g.SearchRanges(dst, ranges)
	if lookup == nil {
		return dst
	}
	out := start
	for _, id := range dst[start:] {
		lat, lng, ok := lookup(id)
		if !ok {
			continue
		}
		ll := s2.LatLngFromDegrees(lat, lng)
		if ll.IsValid() && region.ContainsPoint(s2.PointFromLatLng(ll)) {
			dst[out] = id
			out++
		}
	}
	return dst[:out]
}

func (g *GeoIndex) SearchBounds(dst []ImageID, minLat, minLng, maxLat, maxLng float64, lookup CoordinateLookup) []ImageID {
	lo := s2.LatLngFromDegrees(minLat, minLng)
	hi := s2.LatLngFromDegrees(maxLat, maxLng)
	if !lo.IsValid() || !hi.IsValid() || minLat > maxLat {
		return dst
	}
	rect := s2.RectFromLatLng(lo).AddPoint(hi)
	return g.SearchRegion(dst, rect, lookup)
}

func (g *GeoIndex) SearchCell(dst []ImageID, cellID uint64) []ImageID {
	key, err := S2KeyFromCellID(cellID)
	if err != nil {
		return dst
	}
	return g.SearchRanges(dst, []S2KeyRange{{Start: key, End: key}})
}

func (g *GeoIndex) SearchRanges(dst []ImageID, ranges []S2KeyRange) []ImageID {
	ranges = NormalizeRanges(ranges)
	guard := g.config.Epoch.Enter()
	if !g.closed.Load() {
		view := g.view.Load()
		dst = view.Sparse.SearchRanges(dst, ranges)
		dst = view.Bitmap.SearchRanges(dst, ranges)
	}
	guard.Exit()
	return dst
}

// EstimateRanges returns the number of candidates in the currently published
// sparse and bitmap partitions for ranges. It reads cardinalities only and is
// intended for cross-index query planning.
func (g *GeoIndex) EstimateRanges(ranges []S2KeyRange) uint64 {
	ranges = NormalizeRanges(ranges)
	guard := g.config.Epoch.Enter()
	defer guard.Exit()
	if g.closed.Load() {
		return 0
	}
	view := g.view.Load()
	return indexutil.SaturatingAdd(view.Sparse.estimateRanges(ranges), view.Bitmap.estimateRanges(ranges))
}

func (g *GeoIndex) All(dst []ImageID) []ImageID {
	return g.SearchRanges(dst, []S2KeyRange{{Start: 0, End: ^S2Key(0)}})
}
