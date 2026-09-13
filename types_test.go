package imageidx_test

import (
	"testing"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
)

func TestIndexPackagesShareDomainTypes(t *testing.T) {
	rootID := imageidx.ImageID(42)
	var bitmapID bitmapidx.ImageID = rootID
	var geoID bitmapidx.ImageID = bitmapID
	var timestampID timeidx.ImageID = geoID
	if timestampID != rootID {
		t.Fatalf("shared ImageID = %d", timestampID)
	}

	var durability timeidx.Durability = bitmapidx.DurabilitySync
	if durability != bitmapidx.DurabilitySync {
		t.Fatalf("geo Durability = %d", durability)
	}
	if durability != imageidx.DurabilitySync {
		t.Fatalf("shared Durability = %d", durability)
	}

	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 101, Flags: 7,
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bitmap.Close() })
	geo, err := bitmapidx.NewFieldIndex(bitmapidx.Field{
		Type:    bitmapidx.IndexTypeLocation,
		Kind:    bitmapidx.KindGeo,
		Storage: bitmapidx.StoragePerType,
		Geo:     bitmapidx.GeoCodec{Version: 1},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = geo.Close() })
	times, err := timeidx.New(timeidx.Spec{IndexID: 103, Flags: 7, Offset: 1_000}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = times.Close() })

	if bitmap.Metadata().IndexID != 101 || times.Metadata().IndexID != 103 {
		t.Fatal("stable index identity is not exposed consistently")
	}
	if bitmap.Metadata().Flags != 7 || times.Metadata().Flags != 7 {
		t.Fatal("index flags are not exposed consistently")
	}
	if bitmap.Metadata().Generation == 0 || geo.Metadata().Generation == 0 || times.Metadata().Generation == 0 {
		t.Fatal("index generations must be nonzero")
	}

}
