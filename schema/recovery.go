package schema

import (
	"errors"
	"math"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func align64(v uint64) (uint64, error) {
	if v > math.MaxInt64-63 {
		return 0, ErrExhausted
	}
	return (v + 63) &^ uint64(63), nil
}

func (s *Schema) create(defs []NamespaceConfig) error {
	entries := make([]directoryEntry, len(defs))
	end := uint64(metaStart)
	for i, d := range defs {
		entries[i] = directoryEntry{d.ID, d.Name, d.ReserveSize, end}
		end += metadataPairBytes
	}
	dir, err := directoryBytes(entries)
	if err != nil {
		return err
	}
	dirOff := end
	end += uint64(len(dir))
	if err := s.f.Truncate(int64(end)); err != nil {
		return err
	}
	for _, e := range entries {
		var buf [metadataBytes]byte
		m := allocationRecord{generation: 1}
		encodeRecord(buf[:], m)
		if err := s.write(buf[:], e.offset); err != nil {
			return err
		}
		if err := s.write(buf[:], e.offset+metadataBytes); err != nil {
			return err
		}
		a := &namespaceAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: m}
		s.entries = append(s.entries, a)
	}
	if err := s.write(dir, dirOff); err != nil {
		return err
	}
	if err := s.sync(); err != nil {
		return err
	}
	p := superPayload(dirOff, dir, end)
	var sb [superBytes]byte
	if err := storage.EncodeSuperblock(sb[:], storage.SuperblockHeader{Magic: superMagic, Version: SchemaVersion, Generation: 1}, p[:]); err != nil {
		return err
	}
	for slot := 0; slot < 2; slot++ {
		if err := s.write(sb[:], uint64(slot*superBytes)); err != nil {
			return err
		}
	}
	return s.sync()
}

func (s *Schema) commitDirectory(slot int, generation, off uint64, dir []byte, end uint64) error {
	if s.fault != nil {
		if err := s.fault("before superblock"); err != nil {
			return err
		}
	}
	p := superPayload(off, dir, end)
	var sb [superBytes]byte
	if err := storage.EncodeSuperblock(sb[:], storage.SuperblockHeader{Magic: superMagic, Version: SchemaVersion, Generation: generation}, p[:]); err != nil {
		return err
	}
	if err := s.write(sb[:], uint64(slot*superBytes)); err != nil {
		return err
	}
	if err := s.sync(); err != nil {
		return err
	}
	if s.fault != nil {
		return s.fault("after superblock sync")
	}
	return nil
}

func (s *Schema) loadRecord(e directoryEntry) (allocationRecord, error) {
	var pair [metadataPairBytes]byte
	if err := s.read(pair[:], e.offset); err != nil {
		return allocationRecord{}, formatErr(e.id, errors.Join(ErrCorrupt, err))
	}
	a, aOK := decodeRecord(pair[:metadataBytes])
	b, bOK := decodeRecord(pair[metadataBytes:])
	switch {
	case aOK && bOK:
		if a.generation == b.generation {
			if a.generation != 1 || a.reserved != 0 || a.issued != 0 || a.clean || a != b {
				return allocationRecord{}, formatErr(e.id, ErrCorrupt)
			}
			return a, nil
		}
		if (a.generation > b.generation && a.generation-b.generation != 1) ||
			(b.generation > a.generation && b.generation-a.generation != 1) {
			return allocationRecord{}, formatErr(e.id, ErrCorrupt)
		}
		if (a.generation > b.generation && a.reserved < b.reserved) || (b.generation > a.generation && b.reserved < a.reserved) {
			return allocationRecord{}, formatErr(e.id, ErrCorrupt)
		}
		if b.generation > a.generation {
			return b, nil
		}
		return a, nil
	default:
		// Do not roll back to an older valid slot when a newer slot might
		// represent an acknowledged reservation that was later damaged.
		return allocationRecord{}, formatErr(e.id, ErrCorrupt)
	}
}

func (s *Schema) recover(defs []NamespaceConfig) error {
	info, err := s.f.Stat()
	if err != nil {
		return err
	}
	size := uint64(info.Size())
	if size < metaStart {
		return ErrCorrupt
	}
	var slots [2][superBytes]byte
	for i := range slots {
		if err := s.read(slots[i][:], uint64(i*superBytes)); err != nil {
			return errors.Join(ErrCorrupt, err)
		}
	}
	type candidate struct {
		gen, off, length, end uint64
		valid                 bool
	}
	var found [2]candidate
	for i := range slots {
		if zeroRecord(slots[i][:]) {
			continue
		}
		g, off, length, end, e := parseSuper(slots[i][:], size)
		if e != nil {
			if unsupportedVersion(e) {
				return e
			}
			return ErrCorrupt
		}
		found[i] = candidate{g, off, length, end, true}
	}
	best := -1
	for i, c := range found {
		if c.valid && (best < 0 || c.gen > found[best].gen) {
			best = i
		}
	}
	if best < 0 {
		return ErrCorrupt
	}
	if !found[1-best].valid {
		return ErrCorrupt
	}
	if found[best].gen == found[1-best].gen {
		if found[best].gen != 1 || slots[0] != slots[1] {
			return ErrCorrupt
		}
	} else if found[best].gen-found[1-best].gen != 1 {
		return ErrCorrupt
	}
	c := found[best]
	dir := make([]byte, c.length)
	if err := s.read(dir, c.off); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	_, payload, valid := storage.DecodeSuperblock(slots[best][:], superMagic, SchemaVersion)
	if !valid || checkSuperDirectory(payload, dir) != nil {
		return ErrCorrupt
	}
	entries, err := parseDirectory(dir, c.end)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.offset < c.off+uint64(len(dir)) && c.off < e.offset+metadataPairBytes {
			return formatErr(e.id, ErrCorrupt)
		}
	}
	if len(defs) < len(entries) {
		return ErrNamespace
	}
	byName := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		byName[e.name] = struct{}{}
		found := false
		for _, d := range defs {
			if d.ID == e.id {
				if d.Name != e.name {
					return formatErr(e.id, ErrNamespace)
				}
				found = true
				break
			}
		}
		if !found {
			return formatErr(e.id, ErrNamespace)
		}
		m, err := s.loadRecord(e)
		if err != nil {
			return err
		}
		a := &namespaceAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: m}
		if m.clean {
			// Invalidate reuse *durably* before returning any ID to callers.
			if err := s.persist(a, allocationRecord{m.generation + 1, m.reserved, 0, false}); err != nil {
				return err
			}
			a.issued.Store(m.issued)
		} else {
			a.issued.Store(m.reserved)
		}
		a.limit.Store(m.reserved)
		s.entries = append(s.entries, a)
	}
	changed := false
	for _, d := range defs {
		if _, exists := byName[d.Name]; exists {
			a := s.entry(d.ID)
			if a.block != d.ReserveSize {
				a.block = d.ReserveSize
				changed = true
			}
		} else {
			if s.entry(d.ID) != nil {
				return formatErr(d.ID, ErrNamespace)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if c.gen == math.MaxUint64 {
		return ErrExhausted
	}
	// Use the physical EOF: uncommitted crash tails are never reused. Extend
	// only during Open, before creating a read-write mapping.
	end, err := align64(size)
	if err != nil {
		return err
	}
	newEntries := make([]directoryEntry, 0, len(defs))
	for _, d := range defs {
		if a := s.entry(d.ID); a != nil {
			newEntries = append(newEntries, directoryEntry{d.ID, d.Name, d.ReserveSize, a.offset})
			continue
		}
		if end > math.MaxInt64-metadataPairBytes {
			return ErrExhausted
		}
		off := end
		end += metadataPairBytes
		newEntries = append(newEntries, directoryEntry{d.ID, d.Name, d.ReserveSize, off})
	}
	// Reserve records first, then directory, then publish the inactive SB.
	// Entries are sorted by ID even when additions were supplied out of order.
	for i := range newEntries {
		for j := i + 1; j < len(newEntries); j++ {
			if newEntries[j].id < newEntries[i].id {
				newEntries[i], newEntries[j] = newEntries[j], newEntries[i]
			}
		}
	}
	newDir, err := directoryBytes(newEntries)
	if err != nil {
		return err
	}
	dirOff := end
	end += uint64(len(newDir))
	if end > math.MaxInt64 {
		return ErrExhausted
	}
	if err := s.f.Truncate(int64(end)); err != nil {
		return err
	}
	for _, e := range newEntries {
		if s.entry(e.id) != nil {
			continue
		}
		var buf [metadataBytes]byte
		encodeRecord(buf[:], allocationRecord{generation: 1})
		if err := s.write(buf[:], e.offset); err != nil {
			return err
		}
		if err := s.write(buf[:], e.offset+metadataBytes); err != nil {
			return err
		}
	}
	if err := s.write(newDir, dirOff); err != nil {
		return err
	}
	if err := s.sync(); err != nil {
		return err
	}
	if s.fault != nil {
		if err := s.fault("after directory sync"); err != nil {
			return err
		}
	}
	if err := s.commitDirectory(1-best, c.gen+1, dirOff, newDir, end); err != nil {
		return err
	}
	oldEntries := s.entries
	s.entries = nil
	for _, e := range newEntries {
		var old *namespaceAllocator
		for _, a := range oldEntries {
			if a.id == e.id {
				old = a
				break
			}
		}
		if old != nil {
			s.entries = append(s.entries, old)
		} else {
			s.entries = append(s.entries, &namespaceAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: allocationRecord{generation: 1}})
		}
	}
	return nil
}

func (s *Schema) persist(a *namespaceAllocator, next allocationRecord) error {
	if a.record.generation == math.MaxUint64 || next.generation != a.record.generation+1 || next.reserved < a.record.reserved || next.issued > next.reserved {
		s.poisoned.Store(true)
		return formatErr(a.id, ErrCorrupt)
	}
	var data [metadataBytes]byte
	encodeRecord(data[:], next)
	slot := (next.generation - 1) & 1 // generation 1 is slot A; 2 is slot B
	if s.fault != nil {
		if err := s.fault("before metadata write"); err != nil {
			s.poisoned.Store(true)
			return err
		}
	}
	if err := s.write(data[:], a.offset+slot*metadataBytes); err != nil {
		s.poisoned.Store(true)
		return formatErr(a.id, err)
	}
	if s.fault != nil {
		if err := s.fault("after metadata write"); err != nil {
			s.poisoned.Store(true)
			return err
		}
	}
	if err := s.sync(); err != nil {
		s.poisoned.Store(true)
		return formatErr(a.id, err)
	}
	if s.fault != nil {
		if err := s.fault("after metadata sync"); err != nil {
			s.poisoned.Store(true)
			return err
		}
	}
	a.record = next
	return nil
}
