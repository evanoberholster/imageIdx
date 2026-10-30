package database

import (
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func fullJournalEntry() index.Entry {
	var hash radixhash.Hash
	hash[0] = 9
	return index.Entry{
		ValueID: 42,
		Times:   []index.TimeValue{{Namespace: 1, Timestamp: 100}},
		Geos:    []index.GeoValue{{Namespace: 2, Key: geoidx.S2Key(7)}},
		Sims:    []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: 8, Quality: 4}}},
		Hashes:  []index.HashValue{{Namespace: 4, Hash: hash}},
		Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: []float32{1.5, -2}}},
		Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("enum")}},
	}
}

func TestJournalV2RoundTripAllFamilies(t *testing.T) {
	before := fullJournalEntry()
	after := fullJournalEntry()
	after.Times[0].Timestamp = 200
	after.Bitmaps[0].Value = bitmapidx.EnumStringValue("next")
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 42}, false, before, after)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if entry.entity != schema.EntityImage || entry.id != 42 || entry.deleted || entry.insert ||
		entry.beforeN != 6 || entry.afterN != 6 {
		t.Fatalf("header = %+v", entry)
	}
	var gotBefore, gotAfter index.Entry
	if err := decodeAssociations(entry.before, entry.beforeN, &gotBefore); err != nil {
		t.Fatal(err)
	}
	if err := decodeAssociations(entry.after, entry.afterN, &gotAfter); err != nil {
		t.Fatal(err)
	}
	gotBefore.ValueID = 42
	gotAfter.ValueID = 42
	assertProjectionEqual(t, "before", gotBefore, before)
	assertProjectionEqual(t, "after", gotAfter, after)
}

func TestJournalV2Golden(t *testing.T) {
	after := index.Entry{
		ValueID: 42,
		Times:   []index.TimeValue{{Namespace: 1, Timestamp: 100}},
		Bitmaps: []index.BitmapValue{{Namespace: 2, Value: bitmapidx.EnumStringValue("a")}},
	}
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 42}, true, index.Entry{}, after)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "020201000000000000002a00000002000000000000001d0100010000000000000064060002110000000000000000000000000161"
	// The CRC is appended by the encoder; compare the deterministic prefix and
	// verify the checksum separately so the fixture stays readable.
	if got := hex.EncodeToString(raw[:len(raw)-4]); got != golden {
		t.Fatalf("golden prefix:\n got %s\nwant %s", got, golden)
	}
	if _, err := decodeMutation(raw); err != nil {
		t.Fatal(err)
	}
}

func TestJournalV2ReinsertCarriesTombstoneDelta(t *testing.T) {
	before := fullJournalEntry()
	after := fullJournalEntry()
	after.Bitmaps[0].Value = bitmapidx.EnumStringValue("next")
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 42}, true, before, after)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if entry.insert || !entry.reinsert || entry.deleted {
		t.Fatalf("flags insert=%t reinsert=%t deleted=%t", entry.insert, entry.reinsert, entry.deleted)
	}
	if entry.beforeN != 6 || entry.afterN != 6 {
		t.Fatalf("association counts = %d/%d", entry.beforeN, entry.afterN)
	}
}

func TestJournalV2RejectsMalformed(t *testing.T) {
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 42}, true, index.Entry{}, fullJournalEntry())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "truncated", raw: raw[:len(raw)-1]},
		{name: "future-version", raw: mutateByte(raw, 0, 3)},
		{name: "bad-flags", raw: mutateByte(raw, 1, 0x80)},
		{name: "both-insert-and-reinsert", raw: mutateByte(raw, 1, journalFlagInsert|journalFlagReinsert)},
		{name: "zero-id", raw: mutateByte(raw, 10, 0)},
		{name: "trailing", raw: append(append([]byte(nil), raw...), 0)},
		{name: "bad-crc", raw: mutateByte(raw, len(raw)-1, raw[len(raw)-1]^1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeMutation(tc.raw); err == nil {
				t.Fatal("malformed journal accepted")
			}
		})
	}
}

func TestJournalV2RejectsMalformedAssociations(t *testing.T) {
	entry := index.Entry{Bitmaps: []index.BitmapValue{{Namespace: 1, Value: bitmapidx.EnumStringValue("a")}}}
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, entry)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeAssociations(decoded.after[:len(decoded.after)-1], decoded.afterN, &index.Entry{}); err == nil {
		t.Fatal("truncated association accepted")
	}
	if err := decodeAssociations(decoded.after, 2, &index.Entry{}); err == nil {
		t.Fatal("association count overflow accepted")
	}
	bad := append([]byte(nil), decoded.after...)
	bad[0] = 99
	if err := decodeAssociations(bad, decoded.afterN, &index.Entry{}); err == nil {
		t.Fatal("unknown family accepted")
	}
}

func TestJournalV2DeleteAndNoOpFlags(t *testing.T) {
	deleteRaw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1, Delete: true}, false, index.Entry{}, index.Entry{})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := decodeMutation(deleteRaw)
	if err != nil || !deleted.deleted || deleted.insert || len(deleted.before) != 0 || len(deleted.after) != 0 {
		t.Fatalf("delete = %+v %v", deleted, err)
	}
	noOpRaw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, false, index.Entry{}, index.Entry{})
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := decodeMutation(noOpRaw)
	if err != nil || noOp.deleted || noOp.insert || noOp.beforeN != 0 || noOp.afterN != 0 {
		t.Fatalf("no-op = %+v %v", noOp, err)
	}
}

func mutateByte(raw []byte, index int, value byte) []byte {
	out := append([]byte(nil), raw...)
	out[index] = value
	return out
}

func TestOldJournalVersionFailsClosed(t *testing.T) {
	raw := make([]byte, journalHeader+4)
	raw[0] = 1
	if _, err := decodeMutation(raw); !errors.Is(err, ErrUnsupportedJournalVersion) {
		t.Fatalf("old journal version: %v", err)
	}
}

func TestOldCanonicalVersionFailsClosed(t *testing.T) {
	if _, err := decodeCanonicalValue([]byte{0, 0, 0, 0, 0}); !errors.Is(err, ErrUnsupportedCanonicalVersion) {
		t.Fatalf("old canonical version: %v", err)
	}
}

func TestJournalV2VectorElementKinds(t *testing.T) {
	cases := []index.VectorValue{
		{Namespace: 5, Element: vectoridx.ElementInt8, Int8: []int8{-1, 2}},
		{Namespace: 5, Element: vectoridx.ElementUint8, Uint8: []uint8{1, 255}},
		{Namespace: 5, Element: vectoridx.ElementInt16, Int16: []int16{-300, 300}},
		{Namespace: 5, Element: vectoridx.ElementUint16, Uint16: []uint16{1, 65535}},
		{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: []float32{-1.5, 2.5}},
		{Namespace: 5, Element: vectoridx.ElementFloat16, Float16: []uint16{0x3e00, 0x4100}},
	}
	for _, value := range cases {
		t.Run(fmt.Sprintf("%d", value.Element), func(t *testing.T) {
			entry := index.Entry{ValueID: 1, Vectors: []index.VectorValue{value}}
			raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, entry)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeMutation(raw)
			if err != nil {
				t.Fatal(err)
			}
			var got index.Entry
			if err := decodeAssociations(decoded.after, decoded.afterN, &got); err != nil {
				t.Fatal(err)
			}
			got.ValueID = 1
			assertProjectionEqual(t, "vector", got, entry)
		})
	}
}

func TestJournalV2BitmapKindsRoundTrip(t *testing.T) {
	entries := []index.BitmapValue{
		{Namespace: 6, Value: bitmapidx.EnumStringValue("enum")},
		{Namespace: 6, Value: bitmapidx.BoolValue(true)},
		{Namespace: 6, Value: bitmapidx.IntValue(-12345)},
		{Namespace: 6, Value: bitmapidx.Uint16Value(60000)},
		{Namespace: 6, Value: bitmapidx.Uint32Value(4_000_000_000)},
	}
	for i, value := range entries {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			entry := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{value}}
			raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, entry)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeMutation(raw)
			if err != nil {
				t.Fatal(err)
			}
			var got index.Entry
			if err := decodeAssociations(decoded.after, decoded.afterN, &got); err != nil {
				t.Fatal(err)
			}
			got.ValueID = 1
			assertProjectionEqual(t, "bitmap", got, entry)
		})
	}
}

func TestAppendMutationReuseClearsHeaderFlags(t *testing.T) {
	var scratch []byte
	insert := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("a")}}}
	raw, err := appendMutation(scratch[:0], Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, insert)
	if err != nil {
		t.Fatal(err)
	}
	scratch = raw
	deleteRaw, err := appendMutation(scratch[:0], Mutation{Entity: schema.EntityImage, ValueID: 1, Delete: true}, false, index.Entry{}, index.Entry{})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(deleteRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.deleted || entry.insert {
		t.Fatalf("reused header flags = deleted:%v insert:%v", entry.deleted, entry.insert)
	}
}

func TestJournalV2RejectsInvalidAssociationKinds(t *testing.T) {
	entry := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("a")}}}
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, entry)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	badKind := append([]byte(nil), decoded.after...)
	badKind[3] = 0 // family, namespace, then kind
	if err := decodeAssociations(badKind, decoded.afterN, &index.Entry{}); err == nil {
		t.Fatal("invalid bitmap kind accepted")
	}
	badNS := append([]byte(nil), decoded.after...)
	badNS[1], badNS[2] = 0, 0
	if err := decodeAssociations(badNS, decoded.afterN, &index.Entry{}); err == nil {
		t.Fatal("zero namespace accepted")
	}
	vectorRaw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1}, true, index.Entry{}, index.Entry{
		ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: []float32{1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	vectorEntry, err := decodeMutation(vectorRaw)
	if err != nil {
		t.Fatal(err)
	}
	badVector := append([]byte(nil), vectorEntry.after...)
	badVector[3] = 0
	if err := decodeAssociations(badVector, vectorEntry.afterN, &index.Entry{}); err == nil {
		t.Fatal("invalid vector element accepted")
	}
}
