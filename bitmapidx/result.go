package bitmapidx

import "github.com/evanoberholster/imageIdx/roar"

// Public bitmap results may escape read tracking. Copy for mapped stores;
// in-memory bitmaps remain safe to share because GC owns their lifetime.
func ownStoreResult(result roar.Bitmap, indexes []*Index) roar.Bitmap {
	for _, index := range indexes {
		if index != nil && index.owner != nil {
			return result.Clone()
		}
	}
	return result
}

func (i *FieldIndex) ownResult(result *roar.Bitmap) {
	if i.owner != nil {
		*result = result.Clone()
	}
}
