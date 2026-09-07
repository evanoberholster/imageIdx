package geoidx

import (
	"slices"

	"github.com/evanoberholster/imageIdx/idset"
)

// Apply validates and queues one atomic batch of explicit cell moves. A
// threshold crossing schedules background publication; Flush provides an
// immediate visibility boundary.
func (g *Index) Apply(mutations []Mutation) error {
	if len(mutations) == 0 {
		return nil
	}
	seen := make(map[ImageID]struct{}, len(mutations))
	for _, mutation := range mutations {
		if err := validateMutation(mutation); err != nil {
			return err
		}
		if _, duplicate := seen[mutation.ImageID]; duplicate {
			return ErrDuplicateImageID
		}
		seen[mutation.ImageID] = struct{}{}
	}

	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	nextPending := make(map[ImageID]Mutation, len(g.pending)+len(mutations))
	for id, mutation := range g.pending {
		nextPending[id] = mutation
	}
	view := g.view.Load()
	for _, mutation := range mutations {
		if previous, exists := nextPending[mutation.ImageID]; exists {
			if previous.To != mutation.From {
				return ErrInvalidMutation
			}
			mutation.From = previous.From
		} else if mutation.From != 0 && !view.containsAt(mutation.From, mutation.ImageID) {
			return ErrInvalidMutation
		} else if mutation.From == 0 && mutation.To != 0 && view.containsAt(mutation.To, mutation.ImageID) {
			return ErrDuplicateImageID
		}
		if mutation.From == mutation.To {
			delete(nextPending, mutation.ImageID)
		} else {
			nextPending[mutation.ImageID] = mutation
		}
	}
	if g.persistence != nil && g.persistence.wal != nil {
		if err := g.persistence.wal.append(mutations, g.persistence.durability); err != nil {
			return err
		}
	}
	g.pending = nextPending
	if len(g.pending) >= g.config.PublishThreshold {
		select {
		case g.publish <- struct{}{}:
		default:
		}
	}
	return nil
}

func validateMutation(mutation Mutation) error {
	if mutation.ImageID == 0 {
		return ErrInvalidImageID
	}
	if mutation.From == mutation.To {
		return ErrInvalidMutation
	}
	if mutation.From != 0 {
		if _, err := CellIDFromS2Key(mutation.From); err != nil {
			return err
		}
	}
	if mutation.To != 0 {
		if _, err := CellIDFromS2Key(mutation.To); err != nil {
			return err
		}
	}
	return nil
}

// Flush folds all pending moves into replacement immutable shards.
func (g *Index) Flush() error {
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	return g.flushLocked()
}

func (g *Index) flushLocked() error {
	if len(g.pending) == 0 {
		return nil
	}
	old := g.view.Load()
	next := *old
	next.generation++

	// Group pending moves by shard so each shard is rebuilt once and only
	// from the cells its mutations touch.
	byShard := make(map[int]*shardDelta, len(g.pending))
	for _, mutation := range g.pending {
		if mutation.From != 0 {
			delta := deltaForShard(byShard, shardForKey(mutation.From))
			delta.rems[mutation.From] = append(delta.rems[mutation.From], mutation.ImageID)
		}
		if mutation.To != 0 {
			delta := deltaForShard(byShard, shardForKey(mutation.To))
			delta.adds[mutation.To] = append(delta.adds[mutation.To], mutation.ImageID)
		}
	}
	for shardIndex, delta := range byShard {
		previous := old.shards[shardIndex]
		if previous != nil {
			next.sparseCellCount -= uint64(previous.sparse.cellCount())
			next.denseCellCount -= uint64(previous.dense.cellCount())
		}
		replacement := applyShardDelta(previous, delta)
		next.shards[shardIndex] = replacement
		if replacement != nil {
			next.sparseCellCount += uint64(replacement.sparse.cellCount())
			next.denseCellCount += uint64(replacement.dense.cellCount())
		}
	}
	g.view.Store(&next)
	clear(g.pending)
	return nil
}

// shardDelta groups one flush's cell removals and additions for a single
// shard. Each image appears in at most one pending mutation, so IDs are
// unique within and across both maps.
type shardDelta struct {
	rems map[S2Key][]ImageID
	adds map[S2Key][]ImageID
}

func deltaForShard(byShard map[int]*shardDelta, shard int) *shardDelta {
	delta := byShard[shard]
	if delta == nil {
		delta = &shardDelta{rems: make(map[S2Key][]ImageID), adds: make(map[S2Key][]ImageID)}
		byShard[shard] = delta
	}
	return delta
}

// applyShardDelta folds one shard's cell deltas into a replacement shard.
// Untouched cells are reused by reference (dense) or copied (sparse slots);
// only affected cells are recomputed. Dense updates go through idset.Builder
// over the existing bitmap, so neither ToArray materialization nor a re-sort
// touches large cells. It returns nil when no cells remain.
func applyShardDelta(previous *geoShard, delta *shardDelta) *geoShard {
	var oldSparse sparseRef
	var oldDense bitmapRef
	if previous != nil {
		oldSparse, oldDense = previous.sparse, previous.dense
	}
	affected := make([]S2Key, 0, len(delta.rems)+len(delta.adds))
	for key := range delta.rems {
		affected = append(affected, key)
	}
	for key := range delta.adds {
		if _, ok := delta.rems[key]; !ok {
			affected = append(affected, key)
		}
	}
	slices.Sort(affected)

	promotions := make(map[S2Key]idset.Ref)
	sparseKeys := make([]S2Key, 0, len(oldSparse.keys)+len(affected))
	sparseIDs := make([]uint64, 0, (len(oldSparse.keys)+len(affected))*geoSparseSlots)
	emitSparse := func(key S2Key, list []ImageID) {
		switch {
		case len(list) == 0:
			return
		case len(list) > geoSparseSlots:
			promotions[key] = idset.New(list)
			return
		}
		sparseKeys = append(sparseKeys, key)
		start := len(sparseIDs)
		sparseIDs = append(sparseIDs, list...)
		for len(sparseIDs)-start < geoSparseSlots {
			sparseIDs = append(sparseIDs, 0)
		}
	}

	// Sparse walk in key order: reuse untouched slots, recompute affected
	// cells, skip cells owned by the dense walk.
	si, ai := 0, 0
	for si < len(oldSparse.keys) || ai < len(affected) {
		var key S2Key
		var inOld, inAffected bool
		switch {
		case ai >= len(affected):
			key, inOld = oldSparse.keys[si], true
		case si >= len(oldSparse.keys):
			key, inAffected = affected[ai], true
		case oldSparse.keys[si] < affected[ai]:
			key, inOld = oldSparse.keys[si], true
		case oldSparse.keys[si] > affected[ai]:
			key, inAffected = affected[ai], true
		default:
			key, inOld, inAffected = oldSparse.keys[si], true, true
		}
		if inOld {
			si++
		}
		if inAffected {
			ai++
		}
		switch {
		case inOld && !inAffected:
			sparseKeys = append(sparseKeys, key)
			sparseIDs = append(sparseIDs, oldSparse.imageIDs[(si-1)*geoSparseSlots:si*geoSparseSlots]...)
		case inOldDense(oldDense.keys, key):
			continue
		default:
			var base []ImageID
			if inOld {
				base = sparseSlot(oldSparse.imageIDs, si-1)
			}
			emitSparse(key, applyCellDeltas(base, delta.rems[key], delta.adds[key]))
		}
	}

	// Dense walk in key order: reuse untouched bitmaps, fold affected cells
	// through idset.Builder, absorb sparse promotions at their key position.
	var denseKeys []S2Key
	var denseBitmaps []idset.Ref
	di, ai := 0, 0
	for di < len(oldDense.keys) || ai < len(affected) {
		var key S2Key
		var inOld, inAffected bool
		switch {
		case ai >= len(affected):
			key, inOld = oldDense.keys[di], true
		case di >= len(oldDense.keys):
			key, inAffected = affected[ai], true
		case oldDense.keys[di] < affected[ai]:
			key, inOld = oldDense.keys[di], true
		case oldDense.keys[di] > affected[ai]:
			key, inAffected = affected[ai], true
		default:
			key, inOld, inAffected = oldDense.keys[di], true, true
		}
		if inOld {
			di++
		}
		if inAffected {
			ai++
		}
		if promo, ok := promotions[key]; ok {
			denseKeys = append(denseKeys, key)
			denseBitmaps = append(denseBitmaps, promo)
			continue
		}
		if !inOld {
			continue
		}
		oldRef := oldDense.bitmaps[di-1]
		rems, adds := delta.rems[key], delta.adds[key]
		if len(rems) == 0 && len(adds) == 0 {
			denseKeys = append(denseKeys, key)
			denseBitmaps = append(denseBitmaps, oldRef)
			continue
		}
		updated := updateDenseCell(oldRef, rems, adds)
		if !updated.Valid() {
			continue
		}
		if updated.Cardinality() <= geoSparseSlots {
			demoteSparseCell(&sparseKeys, &sparseIDs, key, updated.ToArray())
			continue
		}
		denseKeys = append(denseKeys, key)
		denseBitmaps = append(denseBitmaps, updated)
	}

	if len(sparseKeys) == 0 && len(denseKeys) == 0 {
		return nil
	}
	return &geoShard{
		sparse: sparseRef{keys: sparseKeys, imageIDs: sparseIDs},
		dense:  bitmapRef{keys: denseKeys, bitmaps: denseBitmaps},
	}
}

// updateDenseCell folds removals and additions into a copy of ref through
// idset.Builder, which stages small batches without allocating and merges in
// one pass at Freeze. The source bitmap is never mutated.
func updateDenseCell(ref idset.Ref, rems, adds []ImageID) idset.Ref {
	builder := idset.Clone(ref)
	for _, id := range rems {
		builder.Remove(id)
	}
	builder.AddMany(adds)
	return builder.Freeze()
}

// demoteSparseCell inserts a shrunken dense cell into the ordered sparse
// staging arrays. Demotions are rare (a flush must remove nearly every ID of
// a dense cell), so binary-search insertion keeps the common path linear.
func demoteSparseCell(sparseKeys *[]S2Key, sparseIDs *[]uint64, key S2Key, list []ImageID) {
	if len(list) == 0 {
		return
	}
	position, _ := slices.BinarySearch(*sparseKeys, key)
	*sparseKeys = slices.Insert(*sparseKeys, position, key)
	slots := make([]uint64, geoSparseSlots)
	copy(slots, list)
	*sparseIDs = slices.Insert(*sparseIDs, position*geoSparseSlots, slots...)
}

// applyCellDeltas returns base minus rems plus adds in ascending order. Base
// is ascending; rems and adds are small. The result is freshly allocated and
// never aliases reader-visible backing.
func applyCellDeltas(base, rems, adds []ImageID) []ImageID {
	out := make([]ImageID, 0, len(base)+len(adds))
	if len(rems) == 0 {
		out = append(out, base...)
	} else {
		if len(rems) > 1 && !slices.IsSorted(rems) {
			slices.Sort(rems)
		}
		for _, id := range base {
			if _, found := slices.BinarySearch(rems, id); !found {
				out = append(out, id)
			}
		}
	}
	out = append(out, adds...)
	slices.Sort(out)
	return slices.Compact(out)
}

// sparseSlot returns the nonzero ID prefix of one sparse slot. Slots are
// stored ascending with zero padding; the result aliases the backing array
// and must not be mutated.
func sparseSlot(imageIDs []uint64, index int) []ImageID {
	values := imageIDs[index*geoSparseSlots : (index+1)*geoSparseSlots]
	end := 0
	for end < len(values) && values[end] != 0 {
		end++
	}
	return values[:end]
}

func inOldDense(keys []S2Key, key S2Key) bool {
	_, found := slices.BinarySearch(keys, key)
	return found
}
