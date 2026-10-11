package timeidx

import (
	"cmp"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/common"
)

type namespaceEntry struct {
	namespace common.Namespace
	index     *Index
	err       error
}
type namespaceRegistry struct{ indexes []namespaceEntry }

// Store is configured during startup, then SealStartup publishes its immutable
// registry. Namespace never acquires the lifecycle mutex. Resolved indexes own
// reader tracking; shutdown disables their operations before draining mappings.
type Store struct {
	mu         sync.Mutex // startup and shutdown only
	dir        string
	config     Config
	durability Durability
	indexes    []namespaceEntry // private startup builder; immutable after sealing
	registry   atomic.Pointer[namespaceRegistry]
	closed     atomic.Bool
}

func NewNamespaceStore(dir string, config Config, durability Durability) (*Store, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, config: cfg, durability: durability}, nil
}

func namespacePosition(entries []namespaceEntry, ns common.Namespace) (int, bool) {
	return slices.BinarySearchFunc(entries, ns, func(e namespaceEntry, ns common.Namespace) int { return cmp.Compare(e.namespace, ns) })
}

// SealStartup ends registration permanently. Call before sharing the Store.
func (s *Store) SealStartup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if s.registry.Load() == nil {
		s.registry.Store(&namespaceRegistry{indexes: s.indexes})
	}
	return nil
}

// Namespace resolves only the published registry. A returned index remains safe
// to call during concurrent shutdown: its operations check the shared closed flag
// and pin mapped data using its own reader tracker.
func (s *Store) Namespace(ns common.Namespace) (*Index, bool) {
	if s.closed.Load() {
		return nil, false
	}
	r := s.registry.Load()
	if r == nil {
		return nil, false
	}
	pos, found := namespacePosition(r.indexes, ns)
	if !found || r.indexes[pos].index == nil || r.indexes[pos].index.isClosed() {
		return nil, false
	}
	return r.indexes[pos].index, true
}

func (s *Store) CreateNamespace(ns common.Namespace) (*Index, error) {
	return s.CreateNamespaceWithSpec(Spec{Namespace: ns})
}

func (s *Store) CreateNamespaceWithSpec(spec Spec) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if s.registry.Load() != nil {
		return nil, ErrNamespaceFrozen
	}
	if !spec.Namespace.Valid() {
		return nil, ErrInvalidNamespace
	}
	pos, found := namespacePosition(s.indexes, spec.Namespace)
	if found {
		return nil, ErrNamespaceExists
	}
	i, err := NewStore(s.dir, spec, s.config, s.durability)
	if err == nil {
		i.ownerClosed = &s.closed
		s.indexes = slices.Insert(s.indexes, pos, namespaceEntry{namespace: spec.Namespace, index: i})
	}
	return i, err
}

// OpenNamespace registers during startup; once sealed it only resolves existing
// members and never opens another file or reassigns a closed member's ID.
func (s *Store) OpenNamespace(ns common.Namespace) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if !ns.Valid() {
		return nil, ErrInvalidNamespace
	}
	pos, found := namespacePosition(s.indexes, ns)
	if found {
		e := s.indexes[pos]
		if e.err != nil {
			return nil, e.err
		}
		if e.index.isClosed() {
			return nil, ErrClosed
		}
		return e.index, nil
	}
	if s.registry.Load() != nil {
		return nil, ErrNamespaceFrozen
	}
	i, err := OpenNamespace(s.dir, ns, s.config, s.durability)
	if err == nil {
		i.ownerClosed = &s.closed
		s.indexes = slices.Insert(s.indexes, pos, namespaceEntry{namespace: ns, index: i})
	}
	return i, err
}

// CloseNamespace is startup-only. Runtime shutdown closes the entire Store.
func (s *Store) CloseNamespace(ns common.Namespace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if s.registry.Load() != nil {
		return ErrNamespaceFrozen
	}
	pos, found := namespacePosition(s.indexes, ns)
	if !found {
		return ErrNamespaceNotFound
	}
	err := s.indexes[pos].index.Close()
	s.indexes = slices.Delete(s.indexes, pos, pos+1)
	return err
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	var err error
	for _, e := range s.indexes {
		if e.index != nil {
			err = errors.Join(err, e.index.Close())
		}
	}
	// Never mutate the published registry, even after shutdown.
	return err
}

// NamespaceInit describes explicit startup intent; Open never implicitly creates
// a missing/corrupt file. Failed members remain configured but unavailable.
type NamespaceInit struct {
	Spec   Spec
	Create bool
	// RequiredSourceGeneration is a DB completeness watermark to check before
	// publishing this namespace. Stale files remain intact but unavailable.
	RequiredSourceGeneration uint64
}
type NamespaceResult struct {
	Namespace common.Namespace
	Err       error
}

// InitializeStore validates the complete configuration before touching namespace
// files, initializes each independently, then seals membership. Per-namespace
// errors are returned separately so a corrupt member cannot block healthy ones.
func InitializeStore(dir string, namespaces []NamespaceInit, config Config, durability Durability) (*Store, []NamespaceResult, error) {
	definitions := slices.Clone(namespaces)
	slices.SortFunc(definitions, func(a, b NamespaceInit) int { return cmp.Compare(a.Spec.Namespace, b.Spec.Namespace) })
	for n, d := range definitions {
		if !d.Spec.Namespace.Valid() {
			return nil, nil, ErrInvalidNamespace
		}
		if n > 0 && definitions[n-1].Spec.Namespace == d.Spec.Namespace {
			return nil, nil, ErrNamespaceExists
		}
	}
	names := slices.Clone(definitions)
	slices.SortFunc(names, func(a, b NamespaceInit) int { return cmp.Compare(a.Spec.Name, b.Spec.Name) })
	for n := 1; n < len(names); n++ {
		if names[n].Spec.Name != "" && names[n].Spec.Name == names[n-1].Spec.Name {
			return nil, nil, ErrNamespaceExists
		}
	}
	s, err := NewNamespaceStore(dir, config, durability)
	if err != nil {
		return nil, nil, err
	}
	if _, err = os.ReadDir(dir); err != nil {
		_ = s.Close()
		return nil, nil, err
	}
	results := make([]NamespaceResult, len(definitions))
	for n, d := range definitions {
		var i *Index
		if d.Create {
			i, err = NewStore(dir, d.Spec, s.config, durability)
		} else {
			i, err = OpenNamespace(dir, d.Spec.Namespace, s.config, durability)
		}
		if i != nil {
			if err = i.CheckSourceGeneration(d.RequiredSourceGeneration); err != nil {
				_ = i.Close()
				i = nil
			}
		}
		if i != nil {
			i.ownerClosed = &s.closed
		}
		s.indexes = append(s.indexes, namespaceEntry{namespace: d.Spec.Namespace, index: i, err: err})
		results[n] = NamespaceResult{Namespace: d.Spec.Namespace, Err: err}
	}
	_ = s.SealStartup()
	return s, results, nil
}

// NamespaceError reports initialization failure without registry locking.
func (s *Store) NamespaceError(ns common.Namespace) error {
	if s.closed.Load() {
		return ErrClosed
	}
	r := s.registry.Load()
	if r == nil {
		return ErrStartupRequired
	}
	pos, found := namespacePosition(r.indexes, ns)
	if !found {
		return ErrNamespaceNotFound
	}
	return r.indexes[pos].err
}
