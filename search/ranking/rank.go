package ranking

import (
	schema "github.com/evanoberholster/imageIdx/schema"
)

// ImageID reuses the repository-wide identifier. It is an alias so existing
// uint64 storage remains zero-conversion.
type ImageID = schema.ImageID

// Rank is one fused or unfused ranking signal. Higher is better. Zero is
// reserved for missing/unranked and sorts after every ordinary value.
type Rank uint16

const (
	// NoRank marks a missing rankable property (no timestamp, GPS,
	// embedding, or text posting). It is worse than any ordinary rank.
	NoRank Rank = 0
	// MinRank is the worst ordinary rank (farthest distance, lowest similarity).
	MinRank Rank = 1
	// MaxRank is the best ordinary rank (exact match, similarity +1).
	MaxRank Rank = 65535
	// BestRank aliases MaxRank for readability at call sites.
	BestRank = MaxRank
)

// Kind names a signal lane for diagnostics and configuration. Fusion must not
// depend on Kind except where an algorithm explicitly documents it.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindTime
	KindGeo
	KindVector
	KindText
	KindIntrinsic
)

// String returns a short human-readable lane name.
func (k Kind) String() string {
	switch k {
	case KindTime:
		return "time"
	case KindGeo:
		return "geo"
	case KindVector:
		return "vector"
	case KindText:
		return "text"
	case KindIntrinsic:
		return "intrinsic"
	default:
		return "unknown"
	}
}

// TimeDistance is a native temporal gap in seconds. uint32 covers ~136 years.
type TimeDistance uint32

// GeoDistance is a native physical distance in centimeters. Earth's maximum
// surface distance fits in uint32.
type GeoDistance uint32
