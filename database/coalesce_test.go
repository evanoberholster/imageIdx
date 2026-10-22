package database

import (
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/golang/geo/s2"
)

func appendTestJournal(t *testing.T, ws *coordinatorWorkspace, sequence, id uint64, before, after []byte, deleted bool) {
	t.Helper()
	raw, err := encodeMutation(Mutation{
		Entity:     schema.EntityImage,
		ValueID:    id,
		Delete:     deleted,
		Projection: after,
	}, before)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.appendMutation(sequence, raw, entry) {
		t.Fatal("appendMutation rejected")
	}
}

func TestCoalescedAddThenUpdate(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	db.mu.Lock()
	db.committed = 2
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendTestJournal(t, ws, 1, 1, nil, projection(1, 'a'), false)
	appendTestJournal(t, ws, 2, 1, projection(1, 'a'), projection(1, 'b'), false)
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 2 {
		t.Fatalf("indexed generation = %d, want 2", got)
	}
	idx, ok := db.indexes.Bitmap(tagNS)
	if !ok {
		t.Fatal("tag namespace missing")
	}
	if containsTag(t, idx, 1, 'a') {
		t.Fatal("stale tag a survived coalescing")
	}
	if !containsTag(t, idx, 1, 'b') {
		t.Fatal("final tag b missing")
	}
}

func TestCoalescedInterleavedEntities(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	db.mu.Lock()
	db.committed = 3
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendTestJournal(t, ws, 1, 1, nil, projection(1, 'a'), false)
	appendTestJournal(t, ws, 2, 2, nil, projection(2, 'x'), false)
	appendTestJournal(t, ws, 3, 1, projection(1, 'a'), projection(1, 'c'), false)
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 3 {
		t.Fatalf("indexed generation = %d, want 3", got)
	}
	idx, _ := db.indexes.Bitmap(tagNS)
	if !containsTag(t, idx, 1, 'c') {
		t.Fatal("entity 1 final tag missing")
	}
	if containsTag(t, idx, 1, 'a') {
		t.Fatal("entity 1 stale tag survived")
	}
	if !containsTag(t, idx, 2, 'x') {
		t.Fatal("entity 2 tag missing")
	}
}

func TestCoalescingFallsBackForDeleteReinsert(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	db.mu.Lock()
	db.committed = 3
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendTestJournal(t, ws, 1, 1, nil, projection(1, 'a'), false)
	appendTestJournal(t, ws, 2, 1, projection(1, 'a'), nil, true)
	appendTestJournal(t, ws, 3, 1, nil, projection(1, 'b'), false)
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 3 {
		t.Fatalf("indexed generation = %d, want 3", got)
	}
	idx, _ := db.indexes.Bitmap(tagNS)
	if !containsTag(t, idx, 1, 'b') {
		t.Fatal("reinserted tag missing")
	}
}

func TestCoalescedUpdateThenUpdate(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	db.committed = 3
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendTestJournal(t, ws, 2, 1, projection(1, 'a'), projection(1, 'b'), false)
	appendTestJournal(t, ws, 3, 1, projection(1, 'b'), projection(1, 'c'), false)
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 3 {
		t.Fatalf("indexed generation = %d, want 3", got)
	}
	idx, _ := db.indexes.Bitmap(tagNS)
	if containsTag(t, idx, 1, 'a') || containsTag(t, idx, 1, 'b') {
		t.Fatal("intermediate tag survived coalescing")
	}
	if !containsTag(t, idx, 1, 'c') {
		t.Fatal("final tag c missing")
	}
}

func TestCoalescedNoOpStillAdvancesGeneration(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	db.committed = 3
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendTestJournal(t, ws, 2, 1, projection(1, 'a'), projection(1, 'b'), false)
	appendTestJournal(t, ws, 3, 1, projection(1, 'b'), projection(1, 'a'), false)
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 3 {
		t.Fatalf("indexed generation = %d, want 3", got)
	}
	idx, _ := db.indexes.Bitmap(tagNS)
	if !containsTag(t, idx, 1, 'a') || containsTag(t, idx, 1, 'b') {
		t.Fatal("A->B->A coalescing did not preserve the initial state")
	}
}

func TestBuildGroupsRejectsUnprovenEngineFamilies(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	ws.count = 2
	ws.mutations[0] = queuedMutation{sequence: 1, entity: schema.EntityImage, id: 1}
	ws.mutations[1] = queuedMutation{sequence: 2, entity: schema.EntityImage, id: 1}
	ws.decoded[0].after.Sims = []index.SimValue{{Namespace: 200}}
	ws.decoded[1].after.Sims = []index.SimValue{{Namespace: 200}}
	if groups, safe := db.buildGroups(ws); safe || len(groups) != 1 {
		t.Fatalf("groups=%d safe=%v, want one unsafe group", len(groups), safe)
	}
}

func containsTag(t *testing.T, idx *bitmapidx.Index, id uint64, value byte) bool {
	t.Helper()
	ok, err := idx.Contains(id, bitmapidx.EnumBytesValue([]byte{value}))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

type randomEvent struct {
	id      uint64
	before  []byte
	after   []byte
	deleted bool
}

func randomBitmapEvents(seed int64, count int) []randomEvent {
	rng := rand.New(rand.NewSource(seed))
	current := make(map[uint64]byte)
	events := make([]randomEvent, 0, count)
	for i := 0; i < count; i++ {
		id := uint64(rng.Intn(4) + 1)
		tag := current[id]
		switch {
		case tag == 0:
			next := byte(rng.Intn(3) + 1)
			current[id] = next
			events = append(events, randomEvent{id: id, after: projection(id, next)})
		case rng.Intn(4) == 0:
			delete(current, id)
			events = append(events, randomEvent{id: id, before: projection(id, tag), deleted: true})
		default:
			next := byte(rng.Intn(3) + 1)
			if next == tag {
				next = tag%3 + 1
			}
			current[id] = next
			events = append(events, randomEvent{id: id, before: projection(id, tag), after: projection(id, next)})
		}
	}
	return events
}

func loadWorkspace(t *testing.T, ws *coordinatorWorkspace, events []randomEvent) {
	t.Helper()
	ws.reset()
	for i, event := range events {
		appendTestJournal(t, ws, uint64(i+1), event.id, event.before, event.after, event.deleted)
	}
}

func TestCoalescingMatchesSequentialRandomBitmap(t *testing.T) {
	for seed := int64(0); seed < 12; seed++ {
		events := randomBitmapEvents(seed, 20)
		seqData, seqIdx := paths(t)
		seqDB := openTest(t, seqData, seqIdx)
		coalData, coalIdx := paths(t)
		coalDB := openTest(t, coalData, coalIdx)

		seqDB.mu.Lock()
		seqDB.committed = uint64(len(events))
		seqDB.mu.Unlock()
		coalDB.mu.Lock()
		coalDB.committed = uint64(len(events))
		coalDB.mu.Unlock()

		seqWS := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
		loadWorkspace(t, seqWS, events)
		if err := seqDB.decodeWorkspace(seqWS); err != nil {
			t.Fatal(err)
		}
		if err := seqDB.applySequentialGroups(seqWS, seqDB.indexes.NewBatch(64)); err != nil {
			t.Fatal(err)
		}

		coalWS := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
		loadWorkspace(t, coalWS, events)
		if err := coalDB.applyWorkspace(coalWS, coalDB.indexes.NewBatch(64)); err != nil {
			t.Fatal(err)
		}

		seqTags, _ := seqDB.indexes.Bitmap(tagNS)
		coalTags, _ := coalDB.indexes.Bitmap(tagNS)
		for id := uint64(1); id <= 4; id++ {
			if got, want := isLive(t, coalDB, schema.EntityImage, id), isLive(t, seqDB, schema.EntityImage, id); got != want {
				t.Fatalf("seed %d id %d: coalesced live=%v sequential live=%v", seed, id, got, want)
			}
			for tag := byte(1); tag <= 3; tag++ {
				if got, want := containsTag(t, coalTags, id, tag), containsTag(t, seqTags, id, tag); got != want {
					// Dead associations need not be physically identical when a
					// delete/reinsert group is applied sequentially.
					if !isLive(t, seqDB, schema.EntityImage, id) {
						continue
					}
					t.Fatalf("seed %d id %d tag %d: coalesced=%v sequential=%v", seed, id, tag, got, want)
				}
			}
		}
		if got, want := coalDB.IndexedGeneration(), seqDB.IndexedGeneration(); got != want {
			t.Fatalf("seed %d generation: coalesced=%d sequential=%d", seed, got, want)
		}
		if err := seqDB.Close(); err != nil {
			t.Fatal(err)
		}
		if err := coalDB.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func isLive(t *testing.T, db *DB, entity schema.EntityKind, id uint64) bool {
	t.Helper()
	idx, value, ok := db.indexes.Liveness(entity)
	if !ok {
		t.Fatal("liveness index missing")
	}
	live, err := idx.Contains(id, value)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func familyPaths(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	data, indexDir := filepath.Join(root, "data"), filepath.Join(root, "index")
	defs := schema.NewDefinitions()
	for _, def := range []schema.NamespaceDef{
		{Namespace: 111, Name: "time", Kind: schema.KindTimestamp, Entity: schema.EntityImage},
		{Namespace: 112, Name: "geo", Kind: schema.KindGeo, Entity: schema.EntityImage},
		{Namespace: 113, Name: "hash", Kind: schema.KindHashExact, Entity: schema.EntityImage},
	} {
		if err := defs.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := index.Create(indexDir, index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	return data, indexDir
}

func familyProjection(id, timestamp, geoKey uint64, hash byte) []byte {
	out := make([]byte, 56)
	binary.BigEndian.PutUint64(out[:8], id)
	binary.BigEndian.PutUint64(out[8:16], timestamp)
	binary.BigEndian.PutUint64(out[16:24], geoKey)
	out[24] = hash
	return out
}

func decodeFamilyProjection(p []byte) (index.Entry, error) {
	if len(p) != 56 {
		return index.Entry{}, ErrCorruptJournal
	}
	var hash radixhash.Hash
	copy(hash[:], p[24:56])
	return index.Entry{
		ValueID: binary.BigEndian.Uint64(p[:8]),
		Times:   []index.TimeValue{{Namespace: 111, Timestamp: binary.BigEndian.Uint64(p[8:16])}},
		Geos:    []index.GeoValue{{Namespace: 112, Key: geoidx.S2Key(binary.BigEndian.Uint64(p[16:24]))}},
		Hashes:  []index.HashValue{{Namespace: 113, Hash: hash}},
	}, nil
}

func TestCoalescedTimeGeoHashFamilies(t *testing.T) {
	data, indexDir := familyPaths(t)
	db, err := Open(data, indexDir, decodeFamilyProjection, Options{BatchSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c1 := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.0, -122.0)).Parent(geoidx.S2Level))
	c2 := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(38.0, -121.0)).Parent(geoidx.S2Level))
	c3 := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(39.0, -120.0)).Parent(geoidx.S2Level))
	k1, k2, k3 := uint64(geoidx.EncodeKey(c1)), uint64(geoidx.EncodeKey(c2)), uint64(geoidx.EncodeKey(c3))
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: familyProjection(1, 100, k1, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	db.committed = 3
	db.mu.Unlock()

	ws := newCoordinatorWorkspace(defaultMaxProjectionBytes, defaultOversizedRetentionBytes)
	ws.reset()
	appendFamilyJournal(t, ws, 2, familyProjection(1, 100, k1, 1), familyProjection(1, 200, k2, 2))
	appendFamilyJournal(t, ws, 3, familyProjection(1, 200, k2, 2), familyProjection(1, 300, k3, 3))
	if err := db.applyWorkspace(ws, db.indexes.NewBatch(64)); err != nil {
		t.Fatal(err)
	}
	if got := db.IndexedGeneration(); got != 3 {
		t.Fatalf("indexed generation = %d, want 3", got)
	}
	timeIdx, ok := db.indexes.Time(111)
	if !ok {
		t.Fatal("time index missing")
	}
	if ids := timeIdx.Search(nil, 300, 301); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("final time = %v", ids)
	}
	if ids := timeIdx.Search(nil, 100, 101); len(ids) != 0 {
		t.Fatalf("stale time survived: %v", ids)
	}
	geoIdx, ok := db.indexes.Geo(112)
	if !ok {
		t.Fatal("geo index missing")
	}
	if ids, err := geoIdx.SearchCell(nil, c3); err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("final geo = %v %v", ids, err)
	}
	if ids, err := geoIdx.SearchCell(nil, c1); err != nil || len(ids) != 0 {
		t.Fatalf("stale geo survived: %v %v", ids, err)
	}
	hashIdx, ok := db.indexes.Hash(113)
	if !ok {
		t.Fatal("hash index missing")
	}
	var finalHash radixhash.Hash
	finalHash[0] = 3
	if ids := hashIdx.Lookup(nil, finalHash); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("final hash = %v", ids)
	}
	var staleHash radixhash.Hash
	staleHash[0] = 1
	if ids := hashIdx.Lookup(nil, staleHash); len(ids) != 0 {
		t.Fatalf("stale hash survived: %v", ids)
	}
}

func appendFamilyJournal(t *testing.T, ws *coordinatorWorkspace, sequence uint64, before, after []byte) {
	t.Helper()
	raw, err := encodeMutation(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: after}, before)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := decodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.appendMutation(sequence, raw, entry) {
		t.Fatal("appendMutation rejected")
	}
}
