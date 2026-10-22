package database

import (
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestDeltaJournalSizeComparison(t *testing.T) {
	for _, projectionLen := range []int{9, 100, 1000} {
		before := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{
			{Namespace: 101, Value: bitmapidx.EnumStringValue("camera")},
			{Namespace: 102, Value: bitmapidx.EnumStringValue("3")},
		}}
		after := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{
			{Namespace: 101, Value: bitmapidx.EnumStringValue("camera")},
			{Namespace: 102, Value: bitmapidx.EnumStringValue("4")},
		}}
		deltaBefore, deltaAfter := changedProjection(before, after)
		journal, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, false, deltaBefore, deltaAfter)
		if err != nil {
			t.Fatal(err)
		}
		recordLen := 100
		v1Journal := 19 + 2*projectionLen + 4
		inline := canonicalHeader + projectionLen + recordLen + len(journal)
		external := canonicalHeader + recordLen + len(journal)
		t.Logf("projection=%d v1-journal=%d v2-journal=%d inline-total=%d external-total=%d",
			projectionLen, v1Journal, len(journal), inline, external)
	}
}
