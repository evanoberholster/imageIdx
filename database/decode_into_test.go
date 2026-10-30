package database

import (
	"encoding/binary"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func decodeAliasInto(encoded []byte, dst *index.Entry, scratch *DecodeScratch) error {
	if len(encoded) != 9 {
		return ErrCorruptJournal
	}
	dst.ValueID = binary.BigEndian.Uint64(encoded[:8])
	// Deliberately alias the coordinator-owned encoded bytes. bitmapidx must
	// copy this value before the workspace block is reused.
	dst.Bitmaps = append(dst.Bitmaps, index.BitmapValue{
		Namespace: tagNS,
		Value:     bitmapidx.EnumBytesValue(encoded[8:9]),
	})
	return nil
}

func TestDecodeIntoAliasedInputIsCopied(t *testing.T) {
	data, idxPath := paths(t)
	db, err := OpenDirs(data, idxPath, nil, Options{BatchSize: 64, DecodeInto: decodeAliasInto})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	idx, ok := db.indexes.Bitmap(tagNS)
	if !ok {
		t.Fatal("tag namespace missing")
	}
	if !containsTag(t, idx, 1, 'a') {
		t.Fatal("aliased decoded value was not copied")
	}

	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'b')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if containsTag(t, idx, 1, 'a') {
		t.Fatal("stale aliased value survived update")
	}
	if !containsTag(t, idx, 1, 'b') {
		t.Fatal("updated aliased value missing")
	}
}
