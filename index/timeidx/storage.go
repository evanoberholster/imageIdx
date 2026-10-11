package timeidx

import (
	"cmp"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	fileMagic          uint32 = 0x54494d34 // TIM4
	fileVersion        uint16 = 4
	payloadBytes              = 64
	directoryEntrySize        = 56
	formatAlignment           = 8
	growAlignment      uint64 = 4096
)

// NamespaceFilename uses four hexadecimal digits for the uint16 namespace.
func NamespaceFilename(ns common.Namespace) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[uint16(ns)>>12], hex[uint16(ns)>>8&15], hex[uint16(ns)>>4&15], hex[uint16(ns)&15]}) + ".timeidx"
}

// NewStore retains the standalone constructor. Namespace zero is supported
// only by this compatibility API; new schema-managed namespaces must be nonzero.
// Neither durability setting weakens the synchronous COW commit guarantee.
func NewStore(dir string, spec Spec, config Config, durability Durability) (*Index, error) {
	return createStore(dir, spec, config, durability, nil)
}

// configure is a per-index test seam, set before any persistence runs.
func createStore(dir string, spec Spec, config Config, durability Durability, configure func(*Index)) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err == nil {
		return nil, ErrUnsupported
	}
	i, err := NewMemory(spec, config)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, NamespaceFilename(spec.Namespace)+".lock", false)
	if err != nil {
		return nil, err
	}
	i.lock = lock
	finalPath := filepath.Join(dir, NamespaceFilename(spec.Namespace))
	if _, err := os.Lstat(finalPath); err == nil {
		_ = storage.ReleaseLock(lock)
		return nil, ErrNamespaceExists
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	// Initialize the ONE data file under a temporary name, then atomically rename
	// that same inode. A crash cannot expose an uninitialized final namespace.
	// The reserved temporary name never denotes an acknowledged namespace.
	i.path = filepath.Join(dir, "."+NamespaceFilename(spec.Namespace)+".creating")
	if err := os.Remove(i.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	if configure != nil {
		configure(i)
	}
	f, err := os.OpenFile(i.path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		if errors.Is(err, os.ErrExist) {
			return nil, ErrNamespaceExists
		}
		return nil, err
	}
	err = f.Truncate(int64(storage.Align(uint64(i.config.MaxFileBytes), growAlignment)))
	err = errors.Join(err, f.Close())
	if err == nil {
		i.file, err = storage.OpenMapped(i.path)
	}
	if err == nil {
		i.alloc, err = storage.ReconstructAllocator(uint64(len(i.file.Bytes())), nil)
	}
	if err == nil {
		i.slot = 1
		err = i.publish(i.view.Load().shards, 1)
	}
	if err == nil {
		err = i.inject("create-name")
	}
	if err == nil {
		err = os.Rename(i.path, finalPath)
		if err == nil {
			i.path = finalPath
		}
	}
	if err == nil {
		err = i.inject("create-sync")
	}
	if err == nil {
		err = storage.SyncDir(dir)
	}
	if err != nil {
		_ = i.Close()
		// Failed final-name publication is never silently replaced. A pre-rename
		// temporary file can be discarded by the next explicit creation attempt.
		return nil, err
	}
	return i, nil
}

// OpenStore opens a standalone directory containing exactly one namespace.
// Multi-namespace callers use Store.OpenNamespace or OpenNamespace directly.
func OpenStore(dir string, config Config, durability Durability) (*Index, error) {
	ns, err := singleNamespace(dir)
	if err != nil {
		return nil, err
	}
	return openNamespace(dir, ns, config, durability)
}

func singleNamespace(dir string) (common.Namespace, error) {
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err == nil {
		return 0, ErrUnsupported
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNamespaceNotFound
		}
		return 0, err
	}
	var found bool
	var ns common.Namespace
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".timeidx" {
			continue
		}
		var n uint16
		if len(e.Name()) != 12 {
			return 0, ErrUnsupported
		}
		for _, c := range e.Name()[:4] {
			n <<= 4
			switch {
			case c >= '0' && c <= '9':
				n += uint16(c - '0')
			case c >= 'a' && c <= 'f':
				n += uint16(c - 'a' + 10)
			default:
				return 0, ErrUnsupported
			}
		}
		if found {
			return 0, ErrAmbiguousNamespace
		}
		found = true
		ns = common.Namespace(n)
	}
	if !found {
		return 0, ErrNamespaceNotFound
	}
	return ns, nil
}

func OpenNamespace(dir string, ns common.Namespace, config Config, durability Durability) (*Index, error) {
	if !ns.Valid() {
		return nil, ErrInvalidNamespace
	}
	return openNamespace(dir, ns, config, durability)
}

func openNamespace(dir string, ns common.Namespace, config Config, durability Durability) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err == nil {
		return nil, ErrUnsupported
	}
	path := filepath.Join(dir, NamespaceFilename(ns))
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNamespaceNotFound
		}
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, NamespaceFilename(ns)+".lock", false)
	if err != nil {
		return nil, err
	}
	mf, err := storage.OpenMapped(path)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		if errors.Is(err, storage.ErrMappedSize) {
			return nil, errors.Join(ErrCorrupt, err)
		}
		return nil, err
	}
	i := &Index{namespace: ns, config: cfg, path: path, lock: lock, file: mf}
	if err = i.load(mf.Bytes()); err != nil {
		_ = mf.Close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return i, nil
}

type generation struct {
	view   *snapshot
	root   storage.Extent
	live   []storage.Extent
	slot   int
	flags  uint32
	offset uint64
}

func extent(offset uint64, size uint32) storage.Extent {
	return storage.Extent{Ref: pagealloc.Ref(offset / uint64(pagealloc.MinBlockSize)), Size: size}
}

func shardLive(shards []*shard, root storage.Extent) []storage.Extent {
	live := make([]storage.Extent, 0, len(shards)+1)
	for _, s := range shards {
		live = append(live, extent(s.offset, uint32(len(s.times)*recordBytes)))
	}
	if root.Size != 0 {
		live = append(live, root)
	}
	return live
}

func decodeGeneration(data, payload []byte, ns common.Namespace, gen uint64) (generation, error) {
	if len(payload) != payloadBytes || gen == 0 {
		return generation{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(payload[:2]) != uint16(ns) {
		return generation{}, ErrNamespaceMismatch
	}
	if binary.LittleEndian.Uint16(payload[2:4]) != 1 {
		return generation{}, ErrUnsupported
	} // exact 1900 epoch
	rootOffset := binary.LittleEndian.Uint64(payload[8:16])
	count := binary.LittleEndian.Uint32(payload[16:20])
	crc := binary.LittleEndian.Uint32(payload[20:24])
	entries := binary.LittleEndian.Uint64(payload[24:32])
	arenaBytes := binary.LittleEndian.Uint64(payload[32:40])
	maxShard := binary.LittleEndian.Uint64(payload[48:56])
	if arenaBytes < 1024 || arenaBytes > uint64(len(data)) || arenaBytes > 1<<41 || arenaBytes%512 != 0 || maxShard < 16 || maxShard > 1<<30 {
		return generation{}, ErrCorrupt
	}
	data = data[:arenaBytes]
	length := uint64(count) * directoryEntrySize
	if length > uint64(^uint32(0))-511 || length > uint64(len(data)) || rootOffset > uint64(len(data))-length {
		return generation{}, ErrCorrupt
	}
	if count == 0 {
		if rootOffset != 0 || crc != storage.Checksum(nil) || entries != 0 {
			return generation{}, ErrCorrupt
		}
		return generation{view: &snapshot{generation: gen}, flags: binary.LittleEndian.Uint32(payload[4:8]), offset: binary.LittleEndian.Uint64(payload[40:48])}, nil
	}
	if rootOffset < 1024 || rootOffset%512 != 0 {
		return generation{}, ErrCorrupt
	}
	dir := data[rootOffset : rootOffset+length]
	if storage.Checksum(dir) != crc {
		return generation{}, ErrCorrupt
	}
	shards := make([]*shard, count)
	var total uint64
	if entries > uint64(len(data))/recordBytes {
		return generation{}, ErrCorrupt
	}
	// Validate uniqueness without a per-entry hash table. Compact ID domains
	// use a bounded bitset; sparse uint64 domains use one sorted scratch column.
	var maxID uint64
	for n := uint32(0); n < count; n++ {
		e := dir[uint64(n)*directoryEntrySize:]
		maxID = max(maxID, binary.LittleEndian.Uint64(e[48:56]))
	}
	var seenBits []uint64
	var seenIDs []uint64
	if maxID <= 8*entries && maxID < 1<<26 {
		seenBits = make([]uint64, (maxID>>6)+1)
	} else {
		seenIDs = make([]uint64, 0, int(entries))
	}
	for n := range shards {
		e := dir[n*directoryEntrySize : (n+1)*directoryEntrySize]
		off := binary.LittleEndian.Uint64(e[16:24])
		num := binary.LittleEndian.Uint32(e[24:28])
		bytes := uint64(num) * recordBytes
		if num == 0 || bytes > maxShard || bytes > uint64(len(data)) || off < 1024 || off%512 != 0 || off > uint64(len(data))-bytes {
			return generation{}, ErrCorrupt
		}
		times, err := storage.View[uint64](data, off, uint64(num), ErrCorrupt, ErrUnsupportedEndian)
		if err != nil {
			return generation{}, err
		}
		ids, err := storage.View[common.ValueID](data, off+uint64(num)*8, uint64(num), ErrCorrupt, ErrUnsupportedEndian)
		if err != nil {
			return generation{}, err
		}
		tcrc, icrc := binary.LittleEndian.Uint32(e[28:32]), binary.LittleEndian.Uint32(e[32:36])
		if storage.Checksum(data[off:off+uint64(num)*8]) != tcrc || storage.Checksum(data[off+uint64(num)*8:off+bytes]) != icrc {
			return generation{}, ErrCorrupt
		}
		s := newShard(times, ids)
		s.offset = off
		s.length = uint32(bytes)
		s.timeCRC = tcrc
		s.idCRC = icrc
		if times[0] != binary.LittleEndian.Uint64(e[:8]) || ids[0] != binary.LittleEndian.Uint64(e[8:16]) || s.minID != binary.LittleEndian.Uint64(e[40:48]) || s.maxID != binary.LittleEndian.Uint64(e[48:56]) {
			return generation{}, ErrCorrupt
		}
		for k, id := range ids {
			if seenBits != nil {
				word, mask := id>>6, uint64(1)<<(id&63)
				if word >= uint64(len(seenBits)) || seenBits[word]&mask != 0 {
					return generation{}, ErrCorrupt
				}
				seenBits[word] |= mask
			} else {
				seenIDs = append(seenIDs, id)
			}
			if k > 0 && !pairLess(times[k-1], ids[k-1], times[k], id) {
				return generation{}, ErrCorrupt
			}
		}
		if n > 0 {
			p := shards[n-1]
			k := len(p.times) - 1
			if !pairLess(p.times[k], p.ids[k], times[0], ids[0]) {
				return generation{}, ErrCorrupt
			}
		}
		shards[n] = s
		total += uint64(num)
	}
	if total != entries {
		return generation{}, ErrCorrupt
	}
	if seenBits == nil {
		slices.Sort(seenIDs)
		for n := 1; n < len(seenIDs); n++ {
			if seenIDs[n] == seenIDs[n-1] {
				return generation{}, ErrCorrupt
			}
		}
	}
	root := extent(rootOffset, uint32(length))
	g := generation{view: &snapshot{shards: shards, generation: gen, count: total}, root: root, live: shardLive(shards, root), flags: binary.LittleEndian.Uint32(payload[4:8]), offset: binary.LittleEndian.Uint64(payload[40:48])}
	if _, err := storage.ReconstructAllocator(arenaBytes, g.live); err != nil {
		return generation{}, errors.Join(ErrCorrupt, err)
	}
	return g, nil
}

func recoverGenerations(data []byte, ns common.Namespace) ([]generation, error) {
	if len(data) < 1024 || len(data)%512 != 0 || uint64(len(data)) > 1<<41 {
		return nil, ErrCorrupt
	}
	var candidates []generation
	for slot := 0; slot < 2; slot++ {
		bytes := data[slot*512 : (slot+1)*512]
		version := binary.LittleEndian.Uint16(bytes[4:6])
		h, p, ok := storage.DecodeSuperblock(bytes, fileMagic, version)
		if !ok {
			continue
		}
		if version != fileVersion {
			return nil, ErrUnsupported
		}
		g, err := decodeGeneration(data, p, ns, h.Generation)
		if errors.Is(err, ErrNamespaceMismatch) || errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnsupportedEndian) {
			return nil, err
		}
		if err != nil {
			continue
		}
		g.slot = slot
		candidates = append(candidates, g)
	}
	slices.SortFunc(candidates, func(a, b generation) int { return cmp.Compare(b.view.generation, a.view.generation) })
	if len(candidates) == 0 {
		return nil, ErrCorrupt
	}
	return candidates, nil
}

func (i *Index) load(data []byte) error {
	gs, err := recoverGenerations(data, i.namespace)
	if err != nil {
		return err
	}
	for n, g := range gs {
		sets := [][]storage.Extent{g.live}
		if n+1 < len(gs) {
			if gs[n+1].view.generation >= g.view.generation {
				return ErrCorrupt
			}
			sets = append(sets, gs[n+1].live)
		}
		live, err := storage.UnionExtents(sets...)
		if err != nil {
			continue
		}
		a, err := storage.ReconstructAllocator(uint64(len(data)), live)
		if err != nil {
			continue
		}
		i.alloc = a
		i.live = live
		i.root = g.root
		i.slot = g.slot
		i.flags = g.flags
		i.offset = g.offset
		i.view.Store(g.view)
		return nil
	}
	return ErrCorrupt
}

func (i *Index) grow(need uint64) error {
	if err := i.inject("grow"); err != nil {
		return err
	}
	oldBytes := uint64(len(i.file.Bytes()))
	target := storage.Align(max(oldBytes*2, oldBytes+need*2), growAlignment)
	if target > 1<<41 || target > uint64(^uint(0)>>1) {
		return pagealloc.ErrTooLarge
	}
	if err := os.Truncate(i.path, int64(target)); err != nil {
		return err
	}
	if err := i.inject("mmap"); err != nil {
		return err
	}
	next, err := storage.OpenMapped(i.path)
	if err != nil {
		return err
	}
	a, err := storage.ReconstructAllocator(target, i.live)
	if err != nil {
		_ = next.Close()
		return err
	}
	i.retired = append(i.retired, i.file)
	i.file = next
	i.alloc = a
	return nil
}

func (i *Index) publish(plans []*shard, gen uint64) error {
	var total uint64
	for _, s := range plans {
		total += uint64(len(s.times))
	}
	if i.file == nil {
		i.view.Store(&snapshot{shards: plans, generation: gen, count: total})
		return nil
	}
	if uint64(len(plans))*directoryEntrySize > uint64(^uint32(0))-511 {
		return ErrInvalidConfig
	}
	var shards []*shard
	var allocated []storage.Extent
	var root storage.Extent
	var dirCRC uint32
	stats := CommitStats{}
	rollback := func() {
		for _, e := range allocated {
			_ = i.alloc.Free(e.Ref, e.Size)
		}
		allocated = nil
	}
	for attempt := 0; ; attempt++ {
		if err := i.inject("allocate"); err != nil {
			return err
		}
		shards = make([]*shard, 0, len(plans))
		stats = CommitStats{}
		var err error
		for _, p := range plans {
			if p.offset != 0 {
				if len(i.retired) == 0 {
					shards = append(shards, p)
					continue
				}
				// Rebind unchanged payloads into the current mapping without copying.
				s := *p
				s.times, err = storage.View[uint64](i.file.Bytes(), p.offset, uint64(len(p.times)), ErrCorrupt, ErrUnsupportedEndian)
				if err == nil {
					s.ids, err = storage.View[common.ValueID](i.file.Bytes(), p.offset+uint64(len(p.times))*8, uint64(len(p.ids)), ErrCorrupt, ErrUnsupportedEndian)
				}
				if err != nil {
					break
				}
				shards = append(shards, &s)
				continue
			}
			size := uint32(len(p.times) * recordBytes)
			ref, capacity, aerr := i.alloc.Alloc(size)
			if aerr != nil {
				err = aerr
				break
			}
			allocated = append(allocated, storage.Extent{Ref: ref, Size: capacity})
			off := uint64(ref) * 512
			times, verr := storage.View[uint64](i.file.Bytes(), off, uint64(len(p.times)), ErrCorrupt, ErrUnsupportedEndian)
			if verr != nil {
				err = verr
				break
			}
			ids, verr := storage.View[common.ValueID](i.file.Bytes(), off+uint64(len(p.times))*8, uint64(len(p.ids)), ErrCorrupt, ErrUnsupportedEndian)
			if verr != nil {
				err = verr
				break
			}
			if err = i.inject("write"); err != nil {
				break
			}
			copy(times, p.times)
			copy(ids, p.ids)
			s := &shard{times: times, ids: ids, minID: p.minID, maxID: p.maxID}
			s.offset = off
			s.length = capacity
			s.timeCRC = storage.Checksum(i.file.Bytes()[off : off+uint64(len(times))*8])
			s.idCRC = storage.Checksum(i.file.Bytes()[off+uint64(len(times))*8 : off+uint64(size)])
			shards = append(shards, s)
			stats.RewrittenShards++
			stats.LogicalBytesWritten += uint64(size)
			stats.AllocatedBytes += uint64(capacity)
		}
		if err == nil && len(shards) != 0 {
			length := uint32(len(shards) * directoryEntrySize)
			ref, capacity, aerr := i.alloc.Alloc(length)
			err = aerr
			if err == nil {
				allocated = append(allocated, storage.Extent{Ref: ref, Size: capacity})
				root = storage.Extent{Ref: ref, Size: length}
				dir := i.file.Bytes()[uint64(ref)*512 : uint64(ref)*512+uint64(length)]
				clear(dir)
				for n, s := range shards {
					e := dir[n*directoryEntrySize : (n+1)*directoryEntrySize]
					binary.LittleEndian.PutUint64(e[:8], s.times[0])
					binary.LittleEndian.PutUint64(e[8:16], s.ids[0])
					binary.LittleEndian.PutUint64(e[16:24], s.offset)
					binary.LittleEndian.PutUint32(e[24:28], uint32(len(s.times)))
					binary.LittleEndian.PutUint32(e[28:32], s.timeCRC)
					binary.LittleEndian.PutUint32(e[32:36], s.idCRC)
					binary.LittleEndian.PutUint64(e[40:48], s.minID)
					binary.LittleEndian.PutUint64(e[48:56], s.maxID)
				}
				dirCRC = storage.Checksum(dir)
				stats.LogicalBytesWritten += uint64(length)
				stats.AllocatedBytes += uint64(capacity)
			}
		} else if err == nil {
			root = storage.Extent{}
			dirCRC = storage.Checksum(nil)
		}
		if err != nil {
			rollback()
			if errors.Is(err, pagealloc.ErrOutOfSpace) && attempt < 8 {
				if err = i.grow(uint64(i.config.MaxShardBytes) + uint64(len(plans)*directoryEntrySize)); err != nil {
					return err
				}
				continue
			}
			return err
		}
		break
	}
	if err := i.inject("directory"); err != nil {
		rollback()
		return err
	}
	if err := i.inject("data-sync"); err != nil {
		rollback()
		return err
	}
	if err := i.file.Sync(); err != nil {
		rollback()
		return err
	}
	newLive := shardLive(shards, root)
	previous := shardLive(i.view.Load().shards, i.root)
	// During creation the heap initial view has no persistent extents.
	if gen == 1 {
		previous = nil
	}
	live, err := storage.UnionExtents(newLive, previous)
	if err != nil {
		rollback()
		return errors.Join(ErrCorrupt, err)
	}
	nextAlloc, err := storage.ReconstructAllocator(uint64(len(i.file.Bytes())), live)
	if err != nil {
		rollback()
		return errors.Join(ErrCorrupt, err)
	}
	var payload [payloadBytes]byte
	binary.LittleEndian.PutUint16(payload[:2], uint16(i.namespace))
	binary.LittleEndian.PutUint16(payload[2:4], 1)
	binary.LittleEndian.PutUint32(payload[4:8], i.flags)
	binary.LittleEndian.PutUint64(payload[8:16], uint64(root.Ref)*512)
	binary.LittleEndian.PutUint32(payload[16:20], uint32(len(shards)))
	binary.LittleEndian.PutUint32(payload[20:24], dirCRC)
	binary.LittleEndian.PutUint64(payload[24:32], total)
	binary.LittleEndian.PutUint64(payload[32:40], uint64(len(i.file.Bytes())))
	binary.LittleEndian.PutUint64(payload[40:48], i.offset)
	binary.LittleEndian.PutUint64(payload[48:56], uint64(max(i.config.MaxShardBytes, largestShard(shards))))
	if err = i.inject("commit"); err != nil {
		rollback()
		return err
	}
	slot := i.slot ^ 1
	outcome, err := storage.CommitSuperblock(storage.SuperblockHeader{Magic: fileMagic, Version: fileVersion, Generation: gen}, payload[:], func(bytes []byte) error {
		if err := i.inject("partial-root"); err != nil {
			copy(i.file.Bytes()[slot*512:slot*512+20], bytes[:20])
			return err
		}
		copy(i.file.Bytes()[slot*512:(slot+1)*512], bytes)
		return nil
	}, func() error {
		if err := i.inject("root-sync"); err != nil {
			return err
		}
		return i.file.Sync()
	})
	if outcome != storage.CommitDurable {
		if outcome == storage.CommitUncertain {
			protected, uerr := storage.UnionExtents(i.live, newLive)
			if uerr != nil {
				return errors.Join(ErrCorrupt, err, uerr)
			}
			i.live = protected // current allocator already reserves all candidate extents
			i.failed = errors.Join(ErrRecoveryRequired, err)
			return i.failed
		}
		rollback()
		return err
	}
	// After durability no fallible step may prevent in-memory publication.
	if i.crash != nil {
		i.crash("publish")
	}
	i.root = root
	i.slot = slot
	i.live = live
	i.view.Store(&snapshot{shards: shards, generation: gen, count: total})
	if i.crash != nil {
		i.crash("reclaim")
	}
	// Keep the old allocator (which still reserves obsolete extents) until ALL
	// readers of the previous view have drained. Only then make reuse possible.
	i.readers.WaitForPriorReaders()
	i.alloc = nextAlloc
	for _, m := range i.retired {
		_ = m.Close()
	}
	i.retired = nil
	stats.LogicalBytesWritten += 512
	stats.FileBytes = uint64(len(i.file.Bytes()))
	i.lastStats = stats
	return nil
}

func largestShard(shards []*shard) int64 {
	var bytes int64
	for _, s := range shards {
		bytes = max(bytes, int64(len(s.times))*recordBytes)
	}
	return bytes
}

// Verify validates a closed single-namespace directory without modifying files.
func Verify(dir string) error {
	ns, err := singleNamespace(dir)
	if err != nil {
		return err
	}
	return VerifyNamespace(dir, ns)
}

func VerifyNamespace(dir string, ns common.Namespace) error {
	data, err := storage.MapFile(filepath.Join(dir, NamespaceFilename(ns)), nil)
	if err != nil {
		return err
	}
	defer storage.Unmap(data)
	i := &Index{namespace: ns}
	return i.load(data)
}
