package bitmapidx

import (
	"bytes"
	"slices"
	"sort"
)

// Dictionary staging bounds. Published descriptors and addressed bytes
// are immutable. The writer may append beyond a published chunk's visible
// length; readers holding an older view keep the older slice length and
// never observe the tail.
const (
	dictChunkTargetBytes    = 1 << 20
	dictChunkInitialBytes   = 4 << 10
	dictActiveTargetEntries = 4096
)

// dictEntry locates one canonical value inside a dictionary view. chunk
// indexes the view's chunk list; offset is a byte offset within that chunk,
// wide enough for snapshots beyond 4 GiB. Entries are ordered by FieldKey;
// layers are append-only once published.
type dictEntry struct {
	key    FieldKey
	chunk  uint32
	offset uint64
	length uint32
}

// dictionaryView is one immutable dictionary snapshot: a compacted base
// plus recently sealed active entries. Lookup precedence is active, then
// base. Chunk bytes are shared with the writer or mmap and must be
// retained via the reader tracker while in use.
type dictionaryView struct {
	base   []dictEntry
	active []dictEntry
	chunks [][]byte
}

// entry returns the highest-precedence entry for key: active first, then
// base.
func (v *dictionaryView) entry(key FieldKey) (dictEntry, bool) {
	if e, ok := findDictEntry(v.active, key); ok {
		return e, true
	}
	return findDictEntry(v.base, key)
}

// bytes returns the canonical bytes for an entry. The result aliases the
// view's chunk storage and is valid only while the view is retained via
// the reader tracker.
func (v *dictionaryView) bytes(e dictEntry) []byte {
	chunk := v.chunks[e.chunk]
	return chunk[e.offset : e.offset+uint64(e.length)]
}

// lookup appends the canonical bytes for key to dst. It reports
// hash-plus-slot presence; callers needing byte equality use resolve.
func (v *dictionaryView) lookup(dst []byte, key FieldKey) ([]byte, bool) {
	e, ok := v.entry(key)
	if !ok {
		return dst, false
	}
	return append(dst, v.bytes(e)...), true
}

// merged returns one entry per key, sorted by key, with active shadowing
// base. It allocates and sorts, so it is O(N log N): use it for
// checkpoints, vacuum, and iteration, never on the query hot path.
func (v *dictionaryView) merged() []dictEntry {
	total := len(v.base) + len(v.active)
	if total == 0 {
		return nil
	}
	entries := make([]dictEntry, 0, total)
	entries = append(entries, v.base...)
	entries = append(entries, v.active...)
	slices.SortStableFunc(entries, func(a, b dictEntry) int { return compareFieldKey(a.key, b.key) })
	out := entries[:0]
	for _, e := range entries {
		if len(out) != 0 && out[len(out)-1].key == e.key {
			continue
		}
		out = append(out, e)
	}
	return out
}

// iterate visits one entry per key in key order. Returning false stops
// early. It materializes merged state, so it allocates.
func (v *dictionaryView) iterate(fn func(key FieldKey, value []byte) bool) {
	for _, e := range v.merged() {
		if !fn(e.key, v.bytes(e)) {
			return
		}
	}
}

// prefix visits entries whose canonical bytes start with prefix, in byte
// order. It materializes and re-sorts by value, so it is O(N log N) and
// allocates; it suits small dictionaries and prefix scans, not hot queries.
func (v *dictionaryView) prefix(prefix []byte, fn func(key FieldKey, value []byte) bool) {
	entries := v.merged()
	slices.SortFunc(entries, func(a, b dictEntry) int {
		return bytes.Compare(v.bytes(a), v.bytes(b))
	})
	for _, e := range entries {
		value := v.bytes(e)
		if !bytes.HasPrefix(value, prefix) {
			continue
		}
		if !fn(e.key, value) {
			return
		}
	}
}

// findDictEntry binary-searches a layer sorted by FieldKey.
func findDictEntry(entries []dictEntry, key FieldKey) (dictEntry, bool) {
	index, ok := slices.BinarySearchFunc(entries, key, func(e dictEntry, k FieldKey) int {
		return compareFieldKey(e.key, k)
	})
	if !ok {
		return dictEntry{}, false
	}
	return entries[index], true
}

// groupEntries returns the contiguous entries sharing base's (Type,
// Hash), ignoring Slot. This works because layers sort by the full
// FieldKey (Type, Hash, Slot), so equal (Type, Hash) prefixes are
// contiguous.
func groupEntries(entries []dictEntry, base FieldKey) []dictEntry {
	start := sort.Search(len(entries), func(index int) bool {
		return compareFieldHash(entries[index].key, base) >= 0
	})
	end := start
	for end < len(entries) && compareFieldHash(entries[end].key, base) == 0 {
		end++
	}
	return entries[start:end]
}

// entriesWithHash returns the entries sharing base's (Type, Hash) across
// all layers, active first. It allocates on every call; assign uses it once
// per new value, which keeps the write path bounded.
func (v *dictionaryView) entriesWithHash(base FieldKey) []dictEntry {
	var out []dictEntry
	for _, layer := range [][]dictEntry{v.active, v.base} {
		out = append(out, groupEntries(layer, base)...)
	}
	return out
}

// resolve returns the key, including its collision slot, of the entry
// whose canonical bytes equal canonical. It scans the hash group in
// precedence order and compares bytes. Layers range over a stack array so
// reads allocate nothing.
func (v *dictionaryView) resolve(base FieldKey, canonical []byte) (FieldKey, bool) {
	for _, layer := range [2][]dictEntry{v.active, v.base} {
		for _, entry := range groupEntries(layer, base) {
			if bytes.Equal(v.bytes(entry), canonical) {
				return entry.key, true
			}
		}
	}
	return FieldKey{}, false
}

func compareFieldKey(a, b FieldKey) int {
	if a.Type != b.Type {
		if a.Type < b.Type {
			return -1
		}
		return 1
	}
	if a.Hash != b.Hash {
		if a.Hash < b.Hash {
			return -1
		}
		return 1
	}
	if a.Slot != b.Slot {
		if a.Slot < b.Slot {
			return -1
		}
		return 1
	}
	return 0
}

// compareFieldHash orders keys by field type and hash, ignoring Slot.
func compareFieldHash(a, b FieldKey) int {
	if a.Type != b.Type {
		if a.Type < b.Type {
			return -1
		}
		return 1
	}
	if a.Hash != b.Hash {
		if a.Hash < b.Hash {
			return -1
		}
		return 1
	}
	return 0
}

// dictionaryWriter owns mutable dictionary state between publications.
// Base and active slices are immutable once published; new values
// accumulate in pending and seal into a fresh active slice. The writer is
// confined to the FieldIndex writer lock and never read concurrently.
type dictionaryWriter struct {
	chunks        [][]byte
	base          []dictEntry
	active        []dictEntry
	pending       []dictEntry
	pendingByHash map[FieldKey][]int
	// generation identifies the visible value set. It advances only when the
	// set changes: folding staged values in seal, or dropping dead values in
	// rebuild. Merges that preserve the set (rotate, compact) leave it alone,
	// so compiled queries can skip re-resolution while it is stable.
	generation uint64
}

// view snapshots the writer for publication. The outer chunk list is
// cloned; the writer may extend its last chunk's slice header afterwards
// while readers keep the older length and only address already-written
// bytes. The clone is O(chunks), and chunks accumulate dead bytes until
// Vacuum rebuilds the writer, so schedules should vacuum large dictionaries
// to bound per-publication allocation.
func (d *dictionaryWriter) view() *dictionaryView {
	return &dictionaryView{base: d.base, active: d.active, chunks: slices.Clone(d.chunks)}
}

// appendBytes copies canonical bytes into the active chunk and returns
// their location. Callers must not mutate b afterwards. The first chunk
// starts small so a field with few values does not reserve the full target;
// chunks grow toward dictChunkTargetBytes, after which a value that would
// exceed it starts a fresh chunk. An oversized value gets its own chunk.
func (d *dictionaryWriter) appendBytes(b []byte) (chunk uint32, offset uint32) {
	if len(d.chunks) == 0 {
		d.chunks = append(d.chunks, make([]byte, 0, max(dictChunkInitialBytes, len(b))))
	}
	last := len(d.chunks) - 1
	if len(d.chunks[last]) > 0 && len(d.chunks[last])+len(b) > dictChunkTargetBytes {
		d.chunks = append(d.chunks, make([]byte, 0, max(dictChunkTargetBytes, len(b))))
		last = len(d.chunks) - 1
	}
	offset = uint32(len(d.chunks[last]))
	d.chunks[last] = append(d.chunks[last], b...)
	return uint32(last), offset
}

// stage copies a new canonical value into the active chunk and queues its
// entry for sealing. It also indexes the pending position by (Type, Hash)
// so same-batch duplicates resolve without scanning.
func (d *dictionaryWriter) stage(key FieldKey, canonical []byte) {
	chunk, offset := d.appendBytes(canonical)
	entry := dictEntry{key: key, chunk: chunk, offset: uint64(offset), length: uint32(len(canonical))}
	d.pending = append(d.pending, entry)
	if d.pendingByHash == nil {
		d.pendingByHash = make(map[FieldKey][]int)
	}
	hashKey := FieldKey{Hash: key.Hash, Type: key.Type}
	d.pendingByHash[hashKey] = append(d.pendingByHash[hashKey], len(d.pending)-1)
}

// pendingBytes returns the canonical bytes of one pending entry. The
// result aliases writer chunk storage owned by the writer lock.
func (d *dictionaryWriter) pendingBytes(entry dictEntry) []byte {
	return d.chunks[entry.chunk][entry.offset : entry.offset+uint64(entry.length)]
}

// assign returns the key for canonical, reusing an existing slot when the
// bytes match or claiming the next free slot for a 64-bit hash collision.
// Newly assigned values are staged immediately. Collision groups are
// almost always size one, so slot tracking is a small-stack scan with no
// map allocation.
func (d *dictionaryWriter) assign(view *dictionaryView, base FieldKey, canonical []byte) (FieldKey, error) {
	hashKey := FieldKey{Hash: base.Hash, Type: base.Type}
	group := view.entriesWithHash(base)
	pending := d.pendingByHash[hashKey]
	if len(group) == 0 && len(pending) == 0 {
		d.stage(hashKey, canonical)
		return hashKey, nil
	}
	var used []uint16
	var useBuf [8]uint16
	used = useBuf[:0]
	track := func(slot uint16) {
		for _, s := range used {
			if s == slot {
				return
			}
		}
		used = append(used, slot)
	}
	for _, entry := range group {
		track(entry.key.Slot)
		if bytes.Equal(view.bytes(entry), canonical) {
			return entry.key, nil
		}
	}
	for _, index := range pending {
		entry := d.pending[index]
		track(entry.key.Slot)
		if bytes.Equal(d.pendingBytes(entry), canonical) {
			return entry.key, nil
		}
	}
	// Overflow to a heap set only for pathological collision storms.
	if len(used) > len(useBuf) {
		set := make(map[uint16]struct{}, len(used))
		for _, s := range used {
			set[s] = struct{}{}
		}
		for slot := uint16(0); ; slot++ {
			if _, exists := set[slot]; !exists {
				key := FieldKey{Hash: base.Hash, Type: base.Type, Slot: slot}
				d.stage(key, canonical)
				return key, nil
			}
			if slot == ^uint16(0) {
				return FieldKey{}, ErrTooManyCollisions
			}
		}
	}
	for slot := uint16(0); ; slot++ {
		claimed := false
		for _, s := range used {
			if s == slot {
				claimed = true
				break
			}
		}
		if !claimed {
			key := FieldKey{Hash: base.Hash, Type: base.Type, Slot: slot}
			d.stage(key, canonical)
			return key, nil
		}
		if slot == ^uint16(0) {
			return FieldKey{}, ErrTooManyCollisions
		}
	}
}

// resolvePending returns the key of a value staged earlier in the same
// batch, so removal-only changes can resolve same-batch additions.
func (d *dictionaryWriter) resolvePending(base FieldKey, canonical []byte) (FieldKey, bool) {
	hashKey := FieldKey{Hash: base.Hash, Type: base.Type}
	for _, index := range d.pendingByHash[hashKey] {
		entry := d.pending[index]
		if bytes.Equal(d.pendingBytes(entry), canonical) {
			return entry.key, true
		}
	}
	return FieldKey{}, false
}

// abort discards entries staged by a batch that failed before
// publication, so a later seal cannot publish values whose postings were
// never written. The caller holds the FieldIndex writer lock.
func (d *dictionaryWriter) abort() {
	d.pending = nil
	d.pendingByHash = nil
}

// seal publishes pending entries into a fresh active slice and folds
// active into base when it exceeds dictActiveTargetEntries. Pending sorts
// once, then merges linearly with the already-sorted active run instead of
// re-sorting the whole layer. Generation advances only when the visible
// set changes, so compiled queries can skip re-resolution while stable.
func (d *dictionaryWriter) seal() {
	if len(d.pending) != 0 {
		slices.SortFunc(d.pending, func(a, b dictEntry) int { return compareFieldKey(a.key, b.key) })
		if len(d.active) == 0 {
			d.active = d.pending
		} else {
			next := make([]dictEntry, 0, len(d.active)+len(d.pending))
			left, right := 0, 0
			for left < len(d.active) && right < len(d.pending) {
				if compareFieldKey(d.active[left].key, d.pending[right].key) <= 0 {
					next = append(next, d.active[left])
					left++
				} else {
					next = append(next, d.pending[right])
					right++
				}
			}
			next = append(next, d.active[left:]...)
			next = append(next, d.pending[right:]...)
			d.active = next
		}
		d.pending = nil
		d.pendingByHash = nil
		d.generation++
	}
	if len(d.active) > dictActiveTargetEntries {
		d.compact()
	}
}

// compact folds active into base with newest precedence and clears
// active. It preserves the visible set, so generation is unchanged.
func (d *dictionaryWriter) compact() {
	if len(d.active) == 0 {
		return
	}
	d.base = mergeDictEntries(d.base, d.active)
	d.active = nil
}

// rebuild replaces writer state with a compacted base built from live
// values only. It advances generation because Vacuum is the one operation
// that can drop dead values from the visible set.
func (d *dictionaryWriter) rebuild(entries []dictEntry, chunks [][]byte) {
	d.base = entries
	d.active = nil
	d.pending = nil
	d.pendingByHash = nil
	d.chunks = chunks
	d.generation++
}

// mergeDictEntries merges two layers sorted by FieldKey, with active
// shadowing base on duplicate keys. Both inputs stay sorted, so the merge
// is a single linear pass with no map and no re-sort.
func mergeDictEntries(base, active []dictEntry) []dictEntry {
	if len(base) == 0 {
		return slices.Clone(active)
	}
	if len(active) == 0 {
		return slices.Clone(base)
	}
	out := make([]dictEntry, 0, len(base)+len(active))
	i, j := 0, 0
	for i < len(base) && j < len(active) {
		cmp := compareFieldKey(base[i].key, active[j].key)
		switch {
		case cmp < 0:
			out = append(out, base[i])
			i++
		case cmp > 0:
			out = append(out, active[j])
			j++
		default:
			// Newest precedence first: keep active, drop base.
			out = append(out, active[j])
			i++
			j++
		}
	}
	out = append(out, base[i:]...)
	out = append(out, active[j:]...)
	return out
}
