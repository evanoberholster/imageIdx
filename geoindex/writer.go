package geoindex

import "slices"

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

	affected := make(map[int]struct{}, len(g.pending)*2)
	for _, mutation := range g.pending {
		if mutation.From != 0 {
			affected[shardForKey(mutation.From)] = struct{}{}
		}
		if mutation.To != 0 {
			affected[shardForKey(mutation.To)] = struct{}{}
		}
	}
	for shardIndex := range affected {
		cells := cellsFromShard(old.shards[shardIndex])
		for _, mutation := range g.pending {
			if mutation.From != 0 && shardForKey(mutation.From) == shardIndex {
				cells[mutation.From] = removeImageID(cells[mutation.From], mutation.ImageID)
				if len(cells[mutation.From]) == 0 {
					delete(cells, mutation.From)
				}
			}
			if mutation.To != 0 && shardForKey(mutation.To) == shardIndex {
				cells[mutation.To] = append(cells[mutation.To], mutation.ImageID)
			}
		}
		previous := old.shards[shardIndex]
		if previous != nil {
			next.sparseCellCount -= uint64(previous.sparse.cellCount())
			next.denseCellCount -= uint64(previous.dense.cellCount())
		}
		if len(cells) == 0 {
			next.shards[shardIndex] = nil
			continue
		}
		replacement := &geoShard{sparse: buildSparse(cells), dense: buildBitmap(cells)}
		next.shards[shardIndex] = replacement
		next.sparseCellCount += uint64(replacement.sparse.cellCount())
		next.denseCellCount += uint64(replacement.dense.cellCount())
	}
	g.view.Store(&next)
	clear(g.pending)
	return nil
}

func cellsFromShard(shard *geoShard) map[S2Key][]ImageID {
	if shard == nil {
		return make(map[S2Key][]ImageID)
	}
	cells := make(map[S2Key][]ImageID, shard.sparse.cellCount()+shard.dense.cellCount())
	for keyIndex, key := range shard.sparse.keys {
		offset := keyIndex * geoSparseSlots
		cells[key] = appendSparseIDs(nil, shard.sparse.imageIDs[offset:offset+geoSparseSlots])
	}
	for keyIndex, key := range shard.dense.keys {
		cells[key] = shard.dense.bitmaps[keyIndex].ToArray()
	}
	return cells
}

func removeImageID(ids []ImageID, target ImageID) []ImageID {
	index := slices.Index(ids, target)
	if index < 0 {
		return ids
	}
	return append(ids[:index], ids[index+1:]...)
}
