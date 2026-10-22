package schema

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func align64(v uint64) (uint64, error) {
	if v > math.MaxInt64-63 {
		return 0, ErrExhausted
	}
	return (v + 63) &^ uint64(63), nil
}

func (s *Schema) create(defs []directoryEntry) error {
	entries := append([]directoryEntry(nil), defs...)
	end := uint64(metaStart)
	for i := range entries {
		entries[i].offset = end
		end += metadataPairBytes
	}
	dir, err := directoryBytes(entries)
	if err != nil {
		return err
	}
	dirOff := end
	end += uint64(len(dir) + len(s.definitionsData))
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
		a := &entityAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: m}
		s.entries = append(s.entries, a)
		s.owners[EntityKind(e.id)] = a
	}
	if err := s.write(dir, dirOff); err != nil {
		return err
	}
	if err := s.write(s.definitionsData, dirOff+uint64(len(dir))); err != nil {
		return err
	}
	if err := s.sync(); err != nil {
		return err
	}
	p := superPayload(dirOff, dir, s.definitionsData, end, s.config)
	var sb [superBytes]byte
	if err := storage.EncodeSuperblock(sb[:], storage.SuperblockHeader{Magic: superMagic, Version: SchemaVersion, Generation: 1}, p[:]); err != nil {
		return err
	}
	for slot := 0; slot < 2; slot++ {
		if err := s.write(sb[:], uint64(slot*superBytes)); err != nil {
			return err
		}
	}
	if err := s.sync(); err != nil {
		return err
	}
	s.generation, s.directoryOffset, s.directoryEnd, s.directory = 1, dirOff, end, dir
	return nil
}

func (s *Schema) commitDirectory(slot int, generation, off uint64, dir, definitions []byte, end uint64, prepared, retired bool) error {
	if s.fault != nil {
		if err := s.fault("before superblock"); err != nil {
			return err
		}
	}
	p := superPayload(off, dir, definitions, end, s.config)
	if prepared {
		p[52] = 1
	}
	if retired {
		p[53] = 1
	}
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

func (s *Schema) recover(defs []directoryEntry, prepare bool) error {
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
	version := binary.LittleEndian.Uint16(slots[best][4:6])
	_, payload, valid := storage.DecodeSuperblock(slots[best][:], superMagic, version)
	if !valid {
		return ErrCorrupt
	}
	if version == SchemaVersion {
		copy(s.config[:], payload[54:60])
	}
	definitionsOffset := binary.LittleEndian.Uint64(payload[32:40])
	definitionsLength := binary.LittleEndian.Uint64(payload[40:48])
	storedDefinitions := make([]byte, definitionsLength)
	if err := s.read(storedDefinitions, definitionsOffset); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	if checkSuperDirectory(payload, dir, storedDefinitions) != nil {
		return ErrCorrupt
	}
	stored := NewDefinitions()
	if err := stored.UnmarshalBinary(storedDefinitions); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	s.generation, s.directoryOffset, s.directoryEnd, s.directory = c.gen, c.off, c.end, dir
	s.legacyRetired = payload[53] == 1
	if payload[52] == 1 {
		previous := found[1-best]
		if previous.gen+1 != c.gen {
			return ErrCorrupt
		}
		_, priorPayload, valid := storage.DecodeSuperblock(slots[1-best][:], superMagic, binary.LittleEndian.Uint16(slots[1-best][4:6]))
		if !valid || priorPayload[52] != 0 {
			return ErrCorrupt
		}
		if priorPayload[53] != payload[53] {
			return ErrCorrupt
		}
		priorDir := make([]byte, previous.length)
		if err := s.read(priorDir, previous.off); err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		priorOffset := binary.LittleEndian.Uint64(priorPayload[32:40])
		priorLength := binary.LittleEndian.Uint64(priorPayload[40:48])
		priorData := make([]byte, priorLength)
		if err := s.read(priorData, priorOffset); err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		if checkSuperDirectory(priorPayload, priorDir, priorData) != nil {
			return ErrCorrupt
		}
		active := NewDefinitions()
		if err := active.UnmarshalBinary(priorData); err != nil {
			return errors.Join(ErrCorrupt, err)
		}
		priorDefs := active.Defs()
		targetDefs := stored.Defs()
		if len(priorDefs) > len(targetDefs) {
			return ErrCorrupt
		}
		changed := len(targetDefs) > len(priorDefs)
		for i, p := range priorDefs {
			d := targetDefs[i]
			if p.Namespace != d.Namespace || p.Name != d.Name || p.Kind != d.Kind || p.Entity != d.Entity || !bytes.Equal(p.Config, d.Config) {
				return ErrCorrupt
			}
			if p.ProjectionVersion != d.ProjectionVersion {
				changed = true
			}
		}
		if !changed {
			return ErrCorrupt
		}
		s.activeDefinitions, s.prepared = active, true
	}
	if s.definitions == nil {
		s.definitions, s.definitionsData = stored, storedDefinitions
	} else {
		prior, desired := stored.Defs(), s.definitions.Defs()
		if len(prior) > len(desired) {
			return fmt.Errorf("%w: requested %d namespaces, persisted %d", ErrNamespace, len(desired), len(prior))
		}
		for i, p := range prior {
			d := desired[i]
			if p.Namespace != d.Namespace || p.Name != d.Name || p.Kind != d.Kind || p.Entity != d.Entity || !bytes.Equal(p.Config, d.Config) {
				return defMismatch(p, d)
			}
		}
	}
	// A standalone additive change that introduces non-KV (index) namespaces
	// needs external index initialization before it can be active. Publish it
	// as PREPARED and require an explicit Activate after those resources are
	// validated. KV-only additions own no dependent storage and may activate
	// immediately.
	if !prepare && !s.prepared {
		prior := stored.Defs()
		current := s.definitions.Defs()
		for _, d := range current[len(prior):] {
			if d.Kind != KindKV {
				prepare = true
				break
			}
		}
	}
	if prepare && !s.prepared && bytes.Equal(storedDefinitions, s.definitionsData) {
		return ErrNamespace // a new generation requires at least one added namespace
	}
	entries, err := parseDirectory(dir, c.end)
	if err != nil {
		return err
	}
	if defs == nil {
		expected, _, _, err := configs(Options{Definitions: stored})
		if err != nil || len(expected) != len(entries) {
			return ErrCorrupt
		}
		for i := range expected {
			if expected[i].id != entries[i].id || expected[i].name != entries[i].name {
				return ErrCorrupt
			}
		}
		defs = entries
	}
	for _, e := range entries {
		if e.offset < c.end && c.off < e.offset+metadataPairBytes {
			return formatErr(e.id, ErrCorrupt)
		}
	}
	if len(defs) < len(entries) {
		return ErrNamespace
	}
	var byEntity [EntityKindCount + 1]bool
	for _, e := range entries {
		if !EntityKind(e.id).Valid() || e.name != EntityKind(e.id).String() {
			return ErrCorrupt
		}
		byEntity[e.id] = true
		found := false
		for _, d := range defs {
			if d.id == e.id {
				if d.name != e.name {
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
		a := &entityAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: m}
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
		s.owners[EntityKind(e.id)] = a
	}
	changed := !bytes.Equal(storedDefinitions, s.definitionsData)
	for _, d := range defs {
		if byEntity[d.id] {
			a := s.entry(EntityKind(d.id))
			if a.block != d.reserve {
				a.block = d.reserve
				changed = true
			}
		} else {
			if s.entry(EntityKind(d.id)) != nil {
				return formatErr(d.id, ErrNamespace)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if s.prepared {
		return ErrNamespace
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
		if a := s.entry(EntityKind(d.id)); a != nil {
			newEntries = append(newEntries, directoryEntry{d.id, d.name, d.reserve, a.offset})
			continue
		}
		if end > math.MaxInt64-metadataPairBytes {
			return ErrExhausted
		}
		off := end
		end += metadataPairBytes
		newEntries = append(newEntries, directoryEntry{d.id, d.name, d.reserve, off})
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
	end += uint64(len(newDir) + len(s.definitionsData))
	if end > math.MaxInt64 {
		return ErrExhausted
	}
	if err := s.f.Truncate(int64(end)); err != nil {
		return err
	}
	for _, e := range newEntries {
		if s.entry(EntityKind(e.id)) != nil {
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
	if err := s.write(s.definitionsData, dirOff+uint64(len(newDir))); err != nil {
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
	if err := s.commitDirectory(1-best, c.gen+1, dirOff, newDir, s.definitionsData, end, prepare, s.legacyRetired); err != nil {
		return err
	}
	s.generation, s.directoryOffset, s.directoryEnd, s.directory = c.gen+1, dirOff, end, newDir
	if prepare {
		s.activeDefinitions, s.prepared = stored, true
	}
	oldEntries := s.entries
	s.entries = nil
	for _, e := range newEntries {
		var old *entityAllocator
		for _, a := range oldEntries {
			if a.id == e.id {
				old = a
				break
			}
		}
		if old != nil {
			s.entries = append(s.entries, old)
		} else {
			a := &entityAllocator{id: e.id, name: e.name, block: e.reserve, offset: e.offset, record: allocationRecord{generation: 1}}
			s.entries = append(s.entries, a)
			s.owners[EntityKind(e.id)] = a
		}
	}
	return nil
}

func (s *Schema) persist(a *entityAllocator, next allocationRecord) error {
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
