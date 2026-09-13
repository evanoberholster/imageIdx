package bitmapidx

import (
	"bytes"
	"slices"
	"sort"
)

// Dictionary staging bounds. Published descriptors and addressed bytes are
// immutable. The writer may append beyond a published chunk's visible length.
const (
	dictChunkTargetBytes    = 1 << 20
	dictActiveTargetEntries = 4096
)

// dictEntry locates one canonical value inside a dictionary view. chunk,
// offset and length index the view's immutable chunk list.
type dictEntry struct {
	key    FieldKey
	chunk  uint32
	offset uint32
	length uint32
}

// dictionaryView is one immutable dictionary snapshot: a compacted base plus
// recently sealed active entries. Lookup precedence is active, then base.
type dictionaryView struct {
	base   []dictEntry
	active []dictEntry
	chunks [][]byte
}

// entry returns the highest-precedence entry for key.
func (v *dictionaryView) entry(key FieldKey) (dictEntry, bool) {
	if e, ok := findDictEntry(v.active, key); ok {
		return e, true
	}
	return findDictEntry(v.base, key)
}

// bytes returns the canonical bytes for an entry. The result aliases the
// view's chunk storage and is valid only while the view is retained.
func (v *dictionaryView) bytes(e dictEntry) []byte {
	chunk := v.chunks[e.chunk]
	return chunk[e.offset : e.offset+e.length]
}

// lookup appends canonical bytes for key to dst.
func (v *dictionaryView) lookup(dst []byte, key FieldKey) ([]byte, bool) {
	e, ok := v.entry(key)
	if !ok {
		return dst, false
	}
	return append(dst, v.bytes(e)...), true
}

// merged returns one entry per key, sorted by key.
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

// iterate visits one entry per key in key order. Returning false stops.
func (v *dictionaryView) iterate(fn func(key FieldKey, value []byte) bool) {
	for _, e := range v.merged() {
		if !fn(e.key, v.bytes(e)) {
			return
		}
	}
}

// prefix visits entries whose canonical bytes start with prefix in byte order.
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

func findDictEntry(entries []dictEntry, key FieldKey) (dictEntry, bool) {
	index, ok := slices.BinarySearchFunc(entries, key, func(e dictEntry, k FieldKey) int {
		return compareFieldKey(e.key, k)
	})
	if !ok {
		return dictEntry{}, false
	}
	return entries[index], true
}

// groupEntries returns the contiguous entries sharing base's (Type, Hash),
// ignoring Slot.
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

// entriesWithHash returns the entries sharing base's (Type, Hash) across all
// layers, highest precedence first.
func (v *dictionaryView) entriesWithHash(base FieldKey) []dictEntry {
	var out []dictEntry
	for _, layer := range [][]dictEntry{v.active, v.base} {
		out = append(out, groupEntries(layer, base)...)
	}
	return out
}

// resolve returns the key of the entry whose canonical bytes equal canonical.
func (v *dictionaryView) resolve(base FieldKey, canonical []byte) (FieldKey, bool) {
	for _, layer := range [][]dictEntry{v.active, v.base} {
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

// dictionaryWriter owns mutable dictionary state between publications. Its
// base/active slices are immutable once published; staging accumulates
// in pending and seals into a fresh active slice. It is confined to the
// FieldIndex writer lock.
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

func (d *dictionaryWriter) view() *dictionaryView {
	// The writer may extend its last chunk's slice header. Readers own an
	// immutable descriptor list and can only address already-written bytes.
	return &dictionaryView{base: d.base, active: d.active, chunks: slices.Clone(d.chunks)}
}

// appendBytes copies canonical bytes into the active chunk and returns their
// location. Callers must not mutate b afterwards.
func (d *dictionaryWriter) appendBytes(b []byte) (chunk uint32, offset uint32) {
	if len(d.chunks) == 0 {
		d.chunks = append(d.chunks, make([]byte, 0, dictChunkTargetBytes))
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

// stage copies a new canonical value and queues its entry for sealing.
func (d *dictionaryWriter) stage(key FieldKey, canonical []byte) {
	chunk, offset := d.appendBytes(canonical)
	entry := dictEntry{key: key, chunk: chunk, offset: offset, length: uint32(len(canonical))}
	d.pending = append(d.pending, entry)
	if d.pendingByHash == nil {
		d.pendingByHash = make(map[FieldKey][]int)
	}
	hashKey := FieldKey{Hash: key.Hash, Type: key.Type}
	d.pendingByHash[hashKey] = append(d.pendingByHash[hashKey], len(d.pending)-1)
}

// pendingBytes returns the canonical bytes of one pending entry.
func (d *dictionaryWriter) pendingBytes(entry dictEntry) []byte {
	return d.chunks[entry.chunk][entry.offset : entry.offset+entry.length]
}

// assign returns the key for canonical, reusing an existing slot when the
// bytes match or claiming the next free slot for a 64-bit hash collision.
// Newly assigned values are staged immediately.
func (d *dictionaryWriter) assign(view *dictionaryView, base FieldKey, canonical []byte) (FieldKey, error) {
	hashKey := FieldKey{Hash: base.Hash, Type: base.Type}
	group := view.entriesWithHash(base)
	pending := d.pendingByHash[hashKey]
	if len(group) == 0 && len(pending) == 0 {
		key := hashKey
		d.stage(key, canonical)
		return key, nil
	}
	used := make(map[uint16]struct{}, len(group)+len(pending))
	match := func(entry dictEntry, value []byte) (FieldKey, bool) {
		used[entry.key.Slot] = struct{}{}
		if bytes.Equal(value, canonical) {
			return entry.key, true
		}
		return FieldKey{}, false
	}
	for _, entry := range group {
		if key, ok := match(entry, view.bytes(entry)); ok {
			return key, nil
		}
	}
	for _, index := range pending {
		entry := d.pending[index]
		if key, ok := match(entry, d.pendingBytes(entry)); ok {
			return key, nil
		}
	}
	slot := uint16(0)
	for {
		if _, exists := used[slot]; !exists {
			break
		}
		if slot == ^uint16(0) {
			return FieldKey{}, ErrTooManyCollisions
		}
		slot++
	}
	key := FieldKey{Hash: base.Hash, Type: base.Type, Slot: slot}
	d.stage(key, canonical)
	return key, nil
}

// resolvePending returns the key of a value staged earlier in the same batch.
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

// abort discards entries staged by a batch that failed before publication,
// so a later seal cannot publish values whose postings were never written.
// Caller holds the FieldIndex writer lock.
func (d *dictionaryWriter) abort() {
	d.pending = nil
	d.pendingByHash = nil
}

// seal publishes pending entries into a fresh active slice and folds layers
// when they exceed their bounds. Pending entries sort once, then merge with
// the already-sorted active run instead of re-sorting the whole layer.
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

// compact folds Active into Base.
func (d *dictionaryWriter) compact() {
	if len(d.active) == 0 {
		return
	}
	d.base = mergeDictEntries(d.base, d.active)
	d.active = nil
}

// rebuild replaces writer state with a compacted base built from live values.
func (d *dictionaryWriter) rebuild(entries []dictEntry, chunks [][]byte) {
	d.base = entries
	d.active = nil
	d.pending = nil
	d.pendingByHash = nil
	d.chunks = chunks
	d.generation++
}

// mergeDictEntries merges sorted layers, newest precedence first, dropping
// duplicate keys.
func mergeDictEntries(base, active []dictEntry) []dictEntry {
	total := len(base) + len(active)
	if total == 0 {
		return nil
	}
	ranked := make([]dictEntry, 0, total)
	ranked = append(ranked, base...)
	ranked = append(ranked, active...)
	precedence := make(map[FieldKey]int, total)
	for index, entry := range ranked {
		precedence[entry.key] = index
	}
	slices.SortFunc(ranked, func(a, b dictEntry) int {
		if cmp := compareFieldKey(a.key, b.key); cmp != 0 {
			return cmp
		}
		return precedence[b.key] - precedence[a.key]
	})
	out := ranked[:0]
	for _, entry := range ranked {
		if len(out) != 0 && out[len(out)-1].key == entry.key {
			continue
		}
		out = append(out, entry)
	}
	return out
}
