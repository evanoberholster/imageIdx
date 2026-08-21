package bitmapidx

import "github.com/dgraph-io/sroar"

// And returns the logical intersection of the supplied immutable snapshots.
// Each operand's Active/Frozen corrections are applied before intersection.
func And(indexes ...*BitmapIndex) BitmapRef {
	if len(indexes) == 0 {
		return EmptyBitmap()
	}
	guards := enterEpochs(indexes)
	defer exitGuards(guards)

	planned := PlanAND(indexes)
	bitmaps := make([]*sroar.Bitmap, 0, len(planned))
	for _, index := range planned {
		if index == nil || index.closed.Load() {
			return EmptyBitmap()
		}
		bitmaps = append(bitmaps, index.view.Load().effectiveBitmap().bitmap)
	}
	if len(bitmaps) == 1 {
		return BitmapRef{bitmap: bitmaps[0]}
	}
	return BitmapRef{bitmap: sroar.FastAnd(bitmaps...)}
}

// Or returns the logical union. Removals remain operand-aware because each
// operand is corrected independently before the union is evaluated.
func Or(indexes ...*BitmapIndex) BitmapRef {
	if len(indexes) == 0 {
		return EmptyBitmap()
	}
	guards := enterEpochs(indexes)
	defer exitGuards(guards)

	bitmaps := make([]*sroar.Bitmap, 0, len(indexes))
	for _, index := range indexes {
		if index == nil || index.closed.Load() {
			continue
		}
		bitmaps = append(bitmaps, index.view.Load().effectiveBitmap().bitmap)
	}
	if len(bitmaps) == 0 {
		return EmptyBitmap()
	}
	if len(bitmaps) == 1 {
		return BitmapRef{bitmap: bitmaps[0]}
	}
	return BitmapRef{bitmap: sroar.FastOr(bitmaps...)}
}

func IntersectInto(dst []ImageID, indexes ...*BitmapIndex) []ImageID {
	for _, id := range And(indexes...).ToArray() {
		dst = append(dst, ImageID(id))
	}
	return dst
}

func UnionInto(dst []ImageID, indexes ...*BitmapIndex) []ImageID {
	for _, id := range Or(indexes...).ToArray() {
		dst = append(dst, ImageID(id))
	}
	return dst
}

func enterEpochs(indexes []*BitmapIndex) []ReadGuard {
	seen := make(map[*Epoch]struct{}, len(indexes))
	guards := make([]ReadGuard, 0, 1)
	for _, index := range indexes {
		if index == nil || index.config.Epoch == nil {
			continue
		}
		if _, exists := seen[index.config.Epoch]; exists {
			continue
		}
		seen[index.config.Epoch] = struct{}{}
		guards = append(guards, index.config.Epoch.Enter())
	}
	return guards
}

func exitGuards(guards []ReadGuard) {
	for i := len(guards) - 1; i >= 0; i-- {
		guards[i].Exit()
	}
}
