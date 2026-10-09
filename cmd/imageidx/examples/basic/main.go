// Command basic runs one combined bitmap + timestamp + geo query.
//
// It mirrors cmd/imageidx runOnce: seed three small indexes, compile a plan
// once, and execute it with a reusable workspace and result buffer.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	namespaceDir, err := os.MkdirTemp("", "imageidx-namespaces-")
	if err != nil {
		log.Fatal(err)
	}
	namespaces, err := bitmapidx.NewStore(namespaceDir, []bitmapidx.NamespaceSpec{
		{Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum},
	}, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = namespaces.Close(); _ = os.RemoveAll(namespaceDir) }()
	favorites, ok := namespaces.Lookup(common.NamespaceTag)
	if !ok {
		log.Fatal("tag namespace missing")
	}
	favoritesBatch := favorites.NewBatch()
	if err := favoritesBatch.Add(
		bitmapidx.Entry{ImageID: 100, Value: bitmapidx.EnumStringValue("favorite")},
		bitmapidx.Entry{ImageID: 200, Value: bitmapidx.EnumStringValue("favorite")},
		bitmapidx.Entry{ImageID: 300, Value: bitmapidx.EnumStringValue("favorite")},
	); err != nil {
		log.Fatal(err)
	}
	if err := favoritesBatch.Apply(); err != nil {
		log.Fatal(err)
	}
	favoritesBatch.Close()
	if err := favorites.Barrier(); err != nil {
		log.Fatal(err)
	}
	geo, err := geoidx.NewMemory(geoidx.Config{Namespace: common.NamespaceLocation})
	if err != nil {
		log.Fatal(err)
	}
	defer geo.Close()
	for _, entry := range []struct {
		id  common.ImageID
		lat float64
		lng float64
	}{
		{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060},
	} {
		key, err := geoidx.KeyFromLatLng(entry.lat, entry.lng)
		if err != nil {
			log.Fatal(err)
		}
		if err := geo.Insert(key, entry.id); err != nil {
			log.Fatal(err)
		}
	}
	if err := geo.Compact(); err != nil {
		log.Fatal(err)
	}

	canonical := map[common.ImageID]uint64{
		100: quantum,
		200: 2 * quantum,
		300: 3 * quantum,
		400: 4 * quantum,
	}
	times, err := timeidx.NewMemory(timeidx.Spec{
		Namespace: common.Namespace(100),
		Name:      "timestamp",
		Entries: []timeidx.Entry{
			{ImageID: 100, Timestamp: canonical[100]},
			{ImageID: 200, Timestamp: canonical[200]},
			{ImageID: 300, Timestamp: canonical[300]},
			{ImageID: 400, Timestamp: canonical[400]},
		},
	}, timeidx.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer times.Close()

	cellID, err := geoidx.CellIDFromLatLng(37.7749, -122.4194)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := search.Compile(
		search.AllValues(favorites, bitmapidx.EnumStringValue("favorite")),
		search.During(times, quantum, 3*quantum,
			search.ExactTimestamp(func(id common.ImageID) (uint64, bool) {
				value, ok := canonical[id]
				return value, ok
			})),
		search.InCells(geo, cellID),
	)
	if err != nil {
		log.Fatal(err)
	}

	workspace := search.NewWorkspace(64)
	matches := make([]search.ImageID, 0, 16)
	matches, err = plan.AppendIDs(context.Background(), matches, workspace)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v stages=%v estimate=%d\n", matches, plan.Stages(), plan.Estimate())
}
