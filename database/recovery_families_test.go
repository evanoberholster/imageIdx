package database

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/golang/geo/s2"
)

func TestRebuildAllFamiliesPreservesTypedValues(t *testing.T) {
	root := t.TempDir()
	data, dir := filepath.Join(root, "data"), filepath.Join(root, "index")
	schema := common.NewSchema()
	defs := []common.NamespaceDef{
		{Namespace: 111, Name: "taken", Kind: common.KindTimestamp, Entity: common.EntityImage},
		{Namespace: 112, Name: "location", Kind: common.KindGeo, Entity: common.EntityImage},
		{Namespace: 113, Name: "sha256", Kind: common.KindHashExact, Entity: common.EntityImage},
		{Namespace: 114, Name: "perceptual", Kind: common.KindHashPerceptual, Entity: common.EntityImage},
	}
	for _, d := range defs {
		if err := schema.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	bsi, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 115, Kind: common.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16}, "score", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(bsi); err != nil {
		t.Fatal(err)
	}
	vector, err := index.VectorDef(vectoridx.Config{Namespace: 116, Dimensions: vectoridx.Dim512, ElementType: vectoridx.ElementInt8}, "embedding", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(vector); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// [id:8][timestamp:8][S2 key:8][rating:2][hash:32][pHash:8]
	// [int8 vector:512]. The codec preserves the original typed vector bytes.
	decode := func(p []byte) (index.Entry, error) {
		if len(p) != 570 {
			return index.Entry{}, ErrCorruptJournal
		}
		var hash radixhash.Hash
		copy(hash[:], p[26:58])
		vec := make([]int8, 512)
		for i, v := range p[66:] {
			vec[i] = int8(v)
		}
		return index.Entry{ValueID: binary.BigEndian.Uint64(p[:8]),
			Times:   []index.TimeValue{{Namespace: 111, Timestamp: binary.BigEndian.Uint64(p[8:16])}},
			Geos:    []index.GeoValue{{Namespace: 112, Key: geoidx.S2Key(binary.BigEndian.Uint64(p[16:24]))}},
			Bitmaps: []index.BitmapValue{{Namespace: 115, Value: bitmapidx.Uint16Value(binary.BigEndian.Uint16(p[24:26]))}},
			Hashes:  []index.HashValue{{Namespace: 113, Hash: hash}},
			Sims:    []index.SimValue{{Namespace: 114, Record: simhash.Record{PHash: binary.BigEndian.Uint64(p[58:66])}}},
			Vectors: []index.VectorValue{{Namespace: 116, Element: vectoridx.ElementInt8, Int8: vec}},
		}, nil
	}
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(geoidx.S2Level)
	projection := make([]byte, 570)
	binary.BigEndian.PutUint64(projection[:8], 1)
	binary.BigEndian.PutUint64(projection[8:16], 123456789)
	binary.BigEndian.PutUint64(projection[16:24], uint64(geoidx.EncodeKey(uint64(cell))))
	binary.BigEndian.PutUint16(projection[24:26], 77)
	projection[26] = 19
	binary.BigEndian.PutUint64(projection[58:66], 808)
	projection[66] = 10
	db, err := Open(data, dir, decode, Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Projection: projection})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "radix-0071.idx")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, dir, decode, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Index().SourceGeneration() != g {
		t.Fatal("rebuild generation mismatch")
	}
	times, ok := db.Index().Time(111)
	if !ok {
		t.Fatal("time missing")
	}
	if ids := times.Search(nil, 123456789, 123456790); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("time=%v", ids)
	}
	geo, ok := db.Index().Geo(112)
	if !ok {
		t.Fatal("geo missing")
	}
	if ids, err := geo.SearchCell(nil, uint64(cell)); err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("geo=%v %v", ids, err)
	}
	rating, ok := db.Index().Bitmap(115)
	if !ok {
		t.Fatal("bsi missing")
	}
	if value, found, err := rating.Get(1); err != nil || !found || value != 77 {
		t.Fatalf("rating=%d %t %v", value, found, err)
	}
	hashes, ok := db.Index().Hash(113)
	if !ok {
		t.Fatal("hash missing")
	}
	var hash radixhash.Hash
	hash[0] = 19
	if ids := hashes.Lookup(nil, hash); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("hash=%v", ids)
	}
	sims, ok := db.Index().Sim(114)
	if !ok {
		t.Fatal("sim missing")
	}
	if rec, found := sims.Get(1); !found || rec.PHash != 808 {
		t.Fatalf("sim=%+v %t", rec, found)
	}
	vectors, ok := db.Index().Vector(116)
	if !ok {
		t.Fatal("vector missing")
	}
	query := make([]int8, 512)
	query[0] = 10
	if results, err := vectors.SearchInt8(query, nil, 1); err != nil || len(results) != 1 || results[0].ValueID != 1 {
		t.Fatalf("vector=%v %v", results, err)
	}
}
