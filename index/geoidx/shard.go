package geoidx

import (
	"sort"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// shardBytesPerRecord is the payload of one (S2Key, ImageID) record: two
// uint64 values. It is the unit used for shard size accounting.
const shardBytesPerRecord = 16

// shard is one immutable contiguous interval of the global (S2Key, ImageID)
// order. keys and ids are parallel, ascending by (key, id), and never mutated
// after publication: compaction replaces a shard wholesale. A persistent shard
// views an mmap arena through mf; a memory shard owns its heap arrays.
type shard struct {
	keys  []S2Key
	ids   []common.ImageID
	mf    *storage.MappedFile
	gen   uint64
	maxID common.ImageID
}

// count is the number of records in the shard.
func (s *shard) count() int { return len(s.keys) }

// bytes is the payload size used for split/merge decisions.
func (s *shard) bytes() int64 { return int64(len(s.keys)) * shardBytesPerRecord }

// firstPair returns the shard's first (key, id). The shard must be non-empty.
func (s *shard) firstPair() (S2Key, common.ImageID) { return s.keys[0], s.ids[0] }

// lastPair returns the shard's last (key, id). The shard must be non-empty.
func (s *shard) lastPair() (S2Key, common.ImageID) {
	n := len(s.keys) - 1
	return s.keys[n], s.ids[n]
}

// keyIDLess orders two (S2Key, ImageID) pairs. Shards and inserts share this
// composite order, so a repeated S2Key is split across shards by ImageID rather
// than by key alone.
func keyIDLess(aKey S2Key, aID common.ImageID, bKey S2Key, bID common.ImageID) bool {
	if aKey != bKey {
		return aKey < bKey
	}
	return aID < bID
}

// shardFor returns the index of the shard that owns insertion (key, id): the last
// shard whose first pair is <= (key, id), clamped to the edges so an insertion
// before the first or after the last shard extends that edge shard. It returns
// -1 for an empty directory.
func shardFor(shards []*shard, key S2Key, id common.ImageID) int {
	if len(shards) == 0 {
		return -1
	}
	// upper bound: first shard whose first pair is strictly greater.
	ub := sort.Search(len(shards), func(i int) bool {
		fk, fi := shards[i].keys[0], shards[i].ids[0]
		return keyIDLess(key, id, fk, fi)
	})
	if ub == 0 {
		return 0
	}
	return ub - 1
}

// firstShardAtOrAfter returns the first shard whose last key is >= start, i.e.
// the first shard that could contain a key in [start, ...]. It returns len(shards)
// when no shard reaches start.
func firstShardAtOrAfter(shards []*shard, start S2Key) int {
	return sort.Search(len(shards), func(i int) bool {
		s := shards[i]
		return s.keys[len(s.keys)-1] >= start
	})
}

// partitionAdds assigns each add to the shard that owns its (key, id), returning
// per-shard sub-slices of adds (which must be sorted by (key, id)) and a dirty
// flag per shard. A single linear pass suffices because both sequences are
// sorted; the shard cursor is monotonic.
func partitionAdds(shards []*shard, adds []mutation) ([][]mutation, []bool) {
	out := make([][]mutation, len(shards))
	dirty := make([]bool, len(shards))
	if len(shards) == 0 || len(adds) == 0 {
		return out, dirty
	}
	si, start := 0, 0
	for start < len(adds) {
		for si+1 < len(shards) {
			fk, fi := shards[si+1].keys[0], shards[si+1].ids[0]
			if keyIDLess(adds[start].key, adds[start].id, fk, fi) {
				break
			}
			si++
		}
		end := start
		for end < len(adds) {
			if si+1 < len(shards) {
				fk, fi := shards[si+1].keys[0], shards[si+1].ids[0]
				if !keyIDLess(adds[end].key, adds[end].id, fk, fi) {
					break
				}
			}
			end++
		}
		out[si] = adds[start:end]
		dirty[si] = true
		start = end
	}
	return out, dirty
}

// splitBounds returns the [start, end) bounds that split n records into chunks of
// at most targetRecords, balancing chunk sizes so no chunk is near-empty. It
// returns nil when n already fits one chunk.
func splitBounds(n, targetRecords int) [][2]int {
	if targetRecords < 1 {
		targetRecords = 1
	}
	chunks := (n + targetRecords - 1) / targetRecords
	if chunks <= 1 {
		return nil
	}
	bounds := make([][2]int, chunks)
	for i := 0; i < chunks; i++ {
		start := i * n / chunks
		end := (i + 1) * n / chunks
		bounds[i] = [2]int{start, end}
	}
	return bounds
}

// validateShardOrder checks that a shard is internally sorted by (key, id) and
// free of reserved zero IDs, returning false when it is malformed.
func validateShardOrder(keys []S2Key, ids []common.ImageID) bool {
	if len(keys) != len(ids) {
		return false
	}
	for n := range keys {
		if ids[n] == 0 {
			return false
		}
		if _, err := CellIDFromKey(keys[n]); err != nil {
			return false
		}
		if n > 0 {
			pk, pi := keys[n-1], ids[n-1]
			if keyIDLess(keys[n], ids[n], pk, pi) || (keys[n] == pk && ids[n] == pi) {
				return false
			}
		}
	}
	return true
}

// validateDirectory checks that non-empty shards are strictly ordered and
// non-overlapping in global (key, id) order.
func validateDirectory(shards []*shard) bool {
	var prevKey S2Key
	var prevID common.ImageID
	for i, s := range shards {
		if s.count() == 0 {
			return false
		}
		if !validateShardOrder(s.keys, s.ids) {
			return false
		}
		fk, fi := s.firstPair()
		if i > 0 && !keyIDLess(prevKey, prevID, fk, fi) {
			return false
		}
		prevKey, prevID = s.lastPair()
	}
	return true
}

// totalEntries sums the base record counts across a directory.
func totalEntries(shards []*shard) int {
	n := 0
	for _, s := range shards {
		n += s.count()
	}
	return n
}
