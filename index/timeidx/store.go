package timeidx

import (
	"errors"
	"os"
	"sync"

	"github.com/evanoberholster/imageIdx/common"
)

// Store owns independently committed namespaces. Its mutex is used ONLY for
// registration/lifecycle operations, never by searches or Index.Apply. Callers
// register each namespace's Spec.NamespaceDef in their common.Schema before
// sharing it with the search planner.
type Store struct {
	mu         sync.Mutex
	dir        string
	config     Config
	durability Durability
	indexes    map[common.Namespace]*Index
	closed     bool
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
	return &Store{dir: dir, config: cfg, durability: durability, indexes: make(map[common.Namespace]*Index)}, nil
}

func (s *Store) CreateNamespace(ns common.Namespace) (*Index, error) {
	return s.CreateNamespaceWithSpec(Spec{Namespace: ns})
}

func (s *Store) CreateNamespaceWithSpec(spec Spec) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if !spec.Namespace.Valid() {
		return nil, ErrInvalidNamespace
	}
	if i := s.indexes[spec.Namespace]; i != nil && !i.closed.Load() {
		return nil, ErrNamespaceExists
	}
	i, err := NewStore(s.dir, spec, s.config, s.durability)
	if err == nil {
		s.indexes[spec.Namespace] = i
	}
	return i, err
}

func (s *Store) OpenNamespace(ns common.Namespace) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if !ns.Valid() {
		return nil, ErrInvalidNamespace
	}
	if i := s.indexes[ns]; i != nil && !i.closed.Load() {
		return i, nil
	}
	i, err := OpenNamespace(s.dir, ns, s.config, s.durability)
	if err == nil {
		s.indexes[ns] = i
	}
	return i, err
}

func (s *Store) CloseNamespace(ns common.Namespace) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	i := s.indexes[ns]
	delete(s.indexes, ns)
	// Hold registration lock until ownership is released, preventing a second
	// mapping racing Close. Ordinary queries in other namespaces remain free.
	defer s.mu.Unlock()
	if i == nil {
		return ErrNamespaceNotFound
	}
	return i.Close()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	for ns, i := range s.indexes {
		err = errors.Join(err, i.Close())
		delete(s.indexes, ns)
	}
	return err
}
