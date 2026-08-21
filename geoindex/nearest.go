package geoindex

import (
	"math"
	"sort"
)

const earthRadiusMeters = 6_371_008.8

type NearestResult struct {
	ImageID        ImageID
	DistanceMeters float64
}

// Nearest returns the exact top K among indexed images for which lookup
// supplies canonical coordinates. V1 uses a full immutable scan; its API is
// compatible with a future expanding-S2-cap executor.
func (g *GeoIndex) Nearest(dst []NearestResult, latitude, longitude float64, k int, lookup CoordinateLookup) []NearestResult {
	if k <= 0 || lookup == nil {
		return dst
	}
	if _, err := KeyFromLatLng(latitude, longitude); err != nil {
		return dst
	}
	start := len(dst)
	for _, id := range g.All(nil) {
		lat, lng, ok := lookup(id)
		if !ok {
			continue
		}
		dst = append(dst, NearestResult{ImageID: id, DistanceMeters: haversineMeters(latitude, longitude, lat, lng)})
	}
	sort.Slice(dst[start:], func(i, j int) bool {
		return dst[start+i].DistanceMeters < dst[start+j].DistanceMeters
	})
	if len(dst)-start > k {
		dst = dst[:start+k]
	}
	return dst
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const radians = math.Pi / 180
	lat1 *= radians
	lat2 *= radians
	dLat := lat2 - lat1
	dLng := (lng2 - lng1) * radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
