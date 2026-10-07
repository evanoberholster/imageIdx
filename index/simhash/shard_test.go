package simhash

import (
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestShardBoundary(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	n := 2*slotsPerHashPage + 5
	records := randRecords(21, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if st.Len() != uint64(n) {
		t.Fatalf("Len = %d, want %d", st.Len(), n)
	}
	if st.ShardCount() != 3 {
		t.Fatalf("ShardCount = %d, want 3", st.ShardCount())
	}
	for _, id := range []uint64{1, slotsPerHashPage - 1, slotsPerHashPage, slotsPerHashPage + 1, 2 * slotsPerHashPage, uint64(n)} {
		got, ok := st.Get(id)
		if !ok || got != records[id-1] {
			t.Fatalf("Get(%d) = %+v, want %+v", id, got, records[id-1])
		}
	}
	target := records[n-1]
	matches := st.SearchPHash(nil, target.PHash, 0)
	found := false
	for _, m := range matches {
		if m.ImageID == uint64(n) {
			found = true
		}
	}
	if !found {
		t.Fatalf("last-shard record not found: %+v", matches)
	}
}

func TestShardBoundaryReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := slotsPerHashPage + 7
	records := randRecords(22, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.ShardCount() != 2 || reopened.Len() != uint64(n) {
		t.Fatalf("reopened shards/len = %d/%d, want 2/%d", reopened.ShardCount(), reopened.Len(), n)
	}
	got, ok := reopened.Get(uint64(n))
	if !ok || got != records[n-1] {
		t.Fatalf("reopened Get(%d) = %+v", n, got)
	}
}

func TestShardGeometryAllSeqBlockSizes(t *testing.T) {
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		geo := newGeometry(seq)
		pps := (defaultMaxShardBytes - headerBytes) / geo.hashPageStride
		if pps == 0 {
			t.Fatalf("seq %d: pages per shard is zero", seq)
		}
		size, ok := shardFileSize(pps, geo)
		if !ok || size > defaultMaxShardBytes {
			t.Fatalf("seq %d: shard size %d exceeds %d", seq, size, defaultMaxShardBytes)
		}
	}
}

func TestShardGeometryRespectsConfiguredMax(t *testing.T) {
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		geo := newGeometry(seq)
		for _, max := range []uint64{minimumShardBytes, 8 << 20, 64 << 20, 1 << 30, defaultMaxShardBytes} {
			if max < uint64(headerBytes)+geo.hashPageStride {
				continue
			}
			pps := (max - headerBytes) / geo.hashPageStride
			size, ok := shardFileSize(pps, geo)
			if !ok || size > max {
				t.Fatalf("seq %d max %d: shard size %d invalid", seq, max, size)
			}
		}
	}
}

func TestShardFileSizeOverflowRejected(t *testing.T) {
	geo := newGeometry(defaultSeqBlockRecords)
	if _, ok := shardFileSize(^uint64(0), geo); ok {
		t.Fatal("overflowing shard size was accepted")
	}
}

func TestMaxShardSize(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{} // default 4 GiB
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	geo := newGeometry(defaultSeqBlockRecords)
	size := uint64(headerBytes) + st.PagesPerShard()*geo.hashPageStride
	if size > defaultMaxShardBytes {
		t.Fatalf("shard size %d exceeds max %d", size, defaultMaxShardBytes)
	}
	if st.PagesPerShard() == 0 {
		t.Fatal("PagesPerShard = 0")
	}
	if st.ShardCount() != 0 {
		t.Fatalf("empty store has %d shards", st.ShardCount())
	}
}

func TestAddressMapping(t *testing.T) {
	cases := []struct {
		id   uint64
		page uint64
		slot int
	}{
		{1, 0, 1},
		{0xffff, 0, 0xffff},
		{0x10000, 1, 0},
		{0x10001, 1, 1},
		{0x1ffff, 1, 0xffff},
		{0x20000, 2, 0},
		{0x1234567, 0x123, 0x4567},
	}
	for _, c := range cases {
		if got := hashPageOf(c.id); got != c.page {
			t.Fatalf("hashPageOf(%#x) = %#x, want %#x", c.id, got, c.page)
		}
		if got := slotOf(c.id); got != c.slot {
			t.Fatalf("slotOf(%#x) = %#x, want %#x", c.id, got, c.slot)
		}
		if got := imageIDFromHashPage(c.page, c.slot); got != c.id {
			t.Fatalf("imageIDFromHashPage(%#x,%#x) = %#x, want %#x", c.page, c.slot, got, c.id)
		}
	}
}

func TestDistances(t *testing.T) {
	if got := DistancePHash(0, 0); got != 0 {
		t.Fatalf("self pHash distance = %d", got)
	}
	if got := DistancePHash(0, 0xf); got != 4 {
		t.Fatalf("pHash distance = %d, want 4", got)
	}
	var a, b PDQHash
	b[0] = 1
	b[31] = 0x80
	if got := DistancePDQ(&a, &a); got != 0 {
		t.Fatalf("self PDQ distance = %d", got)
	}
	if got := DistancePDQ(&a, &b); got != 2 {
		t.Fatalf("PDQ distance = %d, want 2", got)
	}
}

// TestAtomicColumnAlignment checks that every atomic uint64 column access is
// 8-byte aligned, including the boundary slots of an 8-byte quality group and
// the last slot of a hash page.
func TestAtomicColumnAlignment(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(10, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	v := st.view.Load()
	sh, pi := v.shardForHashPage(0)
	for _, slot := range []int{0, 1, 7, 8, 9, 15, 65471, 65528, 65535} {
		if uintptr(unsafe.Pointer(sh.phash(pi, slot)))%8 != 0 {
			t.Fatalf("pHash slot %d not 8-aligned", slot)
		}
		if uintptr(unsafe.Pointer(sh.pdqWord(pi, slot, 0)))%8 != 0 {
			t.Fatalf("PDQ slot %d not 8-aligned", slot)
		}
		if uintptr(unsafe.Pointer(sh.qualityWord(pi, slot)))%8 != 0 {
			t.Fatalf("quality slot %d not 8-aligned", slot)
		}
	}

	// Read-modify-write of each quality byte in the first and last 8-byte group
	// must not disturb its neighbours.
	for _, base := range []int{0, 65528} {
		for s := base; s < base+8; s++ {
			shift := qualityShift(s)
			qw := sh.qualityWord(pi, s)
			atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(s&0xff)<<shift))
		}
		for s := base; s < base+8; s++ {
			if got := uint8(atomic.LoadUint64(sh.qualityWord(pi, s)) >> qualityShift(s)); got != uint8(s&0xff) {
				t.Fatalf("quality slot %d = %#x, want %#x", s, got, uint8(s&0xff))
			}
		}
	}
}

func TestColumnOffsetsAligned(t *testing.T) {
	for name, off := range map[string]uint64{
		"pHash":   pHashColumnOffset,
		"PDQ":     pdqColumnOffset,
		"quality": qualityColumnOffset,
		"seq":     seqOffset,
	} {
		if off%64 != 0 {
			t.Fatalf("%s column offset %d not 64-aligned", name, off)
		}
	}
	if headerBytes%64 != 0 {
		t.Fatal("header not 64-aligned")
	}
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		if s := newGeometry(seq).hashPageStride; s%64 != 0 {
			t.Fatalf("seq %d hash page stride %d not 64-aligned", seq, s)
		}
	}
}
