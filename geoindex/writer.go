package geoindex

type GeoIndexWriter struct{ index *GeoIndex }

func (g *GeoIndex) Writer() GeoIndexWriter { return GeoIndexWriter{index: g} }

func (w GeoIndexWriter) AddBatch(entries []GeoEntry) error     { return w.index.AddBatch(entries) }
func (w GeoIndexWriter) UpdateBatch(entries []GeoUpdate) error { return w.index.UpdateBatch(entries) }
func (w GeoIndexWriter) RemoveBatch(ids []uint64) error        { return w.index.RemoveBatch(ids) }
func (w GeoIndexWriter) WriteBatch(mutations []GeoCellMutation) error {
	return w.index.WriteBatch(mutations)
}
func (w GeoIndexWriter) Flush() error { return w.index.Flush() }

func (g *GeoIndex) AddBatch(entries []GeoEntry) error {
	mutations, err := coordinateMutations(entries)
	if err != nil {
		return err
	}
	return g.queueMutations(mutations, false, false)
}

func (g *GeoIndex) UpdateBatch(entries []GeoUpdate) error {
	mutations, err := coordinateMutations(entries)
	if err != nil {
		return err
	}
	return g.queueMutations(mutations, true, false)
}

func (g *GeoIndex) AddCellBatch(entries []GeoCellEntry) error {
	mutations := make(map[uint64]pendingMutation, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		if _, duplicate := mutations[entry.ImageID]; duplicate {
			return ErrDuplicateImageID
		}
		if _, err := CellIDFromS2Key(entry.Key); err != nil {
			return err
		}
		mutations[entry.ImageID] = pendingMutation{key: entry.Key}
	}
	return g.queueMutations(mutations, false, false)
}

func (g *GeoIndex) RemoveBatch(ids []uint64) error {
	mutations := make(map[uint64]pendingMutation, len(ids))
	for _, id := range ids {
		if id == 0 {
			return ErrInvalidImageID
		}
		if _, duplicate := mutations[id]; duplicate {
			return ErrDuplicateImageID
		}
		mutations[id] = pendingMutation{remove: true}
	}
	return g.queueMutations(mutations, true, true)
}

// WriteBatch applies low-level cell mutations. GeoAdd is an upsert and can
// therefore express a coordinate move in one atomic published generation.
func (g *GeoIndex) WriteBatch(entries []GeoCellMutation) error {
	mutations := make(map[uint64]pendingMutation, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return ErrInvalidImageID
		}
		if _, duplicate := mutations[entry.ImageID]; duplicate {
			return ErrDuplicateImageID
		}
		switch entry.Op {
		case GeoAdd:
			if _, err := CellIDFromS2Key(entry.Key); err != nil {
				return err
			}
			mutations[entry.ImageID] = pendingMutation{key: entry.Key}
		case GeoRemove:
			mutations[entry.ImageID] = pendingMutation{remove: true}
		default:
			return ErrInvalidConfig
		}
	}
	if len(mutations) == 0 {
		return nil
	}
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	if g.wal != nil {
		if err := g.wal.append(mutations, g.durability); err != nil {
			return err
		}
	}
	for id, mutation := range mutations {
		g.pending[id] = mutation
	}
	if len(g.pending) >= g.config.PublishThreshold {
		return g.flushLocked()
	}
	return nil
}

func coordinateMutations(entries []GeoEntry) (map[uint64]pendingMutation, error) {
	mutations := make(map[uint64]pendingMutation, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return nil, ErrInvalidImageID
		}
		if _, duplicate := mutations[entry.ImageID]; duplicate {
			return nil, ErrDuplicateImageID
		}
		key, err := KeyFromLatLng(entry.Latitude, entry.Longitude)
		if err != nil {
			return nil, err
		}
		mutations[entry.ImageID] = pendingMutation{key: key}
	}
	return mutations, nil
}

func (g *GeoIndex) queueMutations(mutations map[uint64]pendingMutation, requireExisting, removal bool) error {
	if len(mutations) == 0 {
		return nil
	}
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	for id := range mutations {
		_, exists := g.effectiveLocationLocked(id)
		if requireExisting && !exists {
			return ErrImageNotFound
		}
		if !requireExisting && exists {
			return ErrDuplicateImageID
		}
	}
	if g.wal != nil {
		if err := g.wal.append(mutations, g.durability); err != nil {
			return err
		}
	}
	for id, mutation := range mutations {
		if removal {
			mutation.remove = true
		}
		g.pending[id] = mutation
	}
	if len(g.pending) >= g.config.PublishThreshold {
		return g.flushLocked()
	}
	return nil
}

func (g *GeoIndex) effectiveLocationLocked(id uint64) (S2Key, bool) {
	if mutation, ok := g.pending[id]; ok {
		if mutation.remove {
			return 0, false
		}
		return mutation.key, true
	}
	key, ok := g.locations[id]
	return key, ok
}

func (g *GeoIndex) Flush() error {
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	return g.flushLocked()
}

func (g *GeoIndex) flushLocked() error {
	if len(g.pending) == 0 {
		return nil
	}
	locations := make(map[uint64]S2Key, len(g.locations)+len(g.pending))
	for id, key := range g.locations {
		locations[id] = key
	}
	for id, mutation := range g.pending {
		if mutation.remove {
			delete(locations, id)
		} else {
			locations[id] = mutation.key
		}
	}
	cells := cellsFromLocations(locations)
	old := g.view.Load()
	next := &GeoIndexView{
		Version: old.Version, Flags: old.Flags, IndexID: old.IndexID, Generation: old.Generation + 1,
		Sparse: buildSparse(cells), Bitmap: buildBitmap(cells),
	}
	g.view.Store(next)
	g.locations = locations
	clear(g.pending)
	g.generation = next.Generation
	return nil
}
