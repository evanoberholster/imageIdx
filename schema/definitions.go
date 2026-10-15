package schema

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"unicode/utf8"
)

var (
	// ErrNamespaceInvalid reports the reserved zero namespace.
	ErrNamespaceInvalid = errors.New("schema: namespace zero is reserved")
	// ErrNamespaceDuplicate reports a namespace number already registered.
	ErrNamespaceDuplicate = errors.New("schema: duplicate namespace")
	// ErrNamespaceName reports an empty or duplicate namespace name.
	ErrNamespaceName = errors.New("schema: namespace name must be non-empty and unique")
	// ErrNamespaceKind reports an unrecognized namespace kind.
	ErrNamespaceKind = errors.New("schema: invalid namespace kind")
	// ErrNamespaceEntity reports an unrecognized (non-zero) entity kind.
	ErrNamespaceEntity = errors.New("schema: invalid namespace entity kind")
	// ErrCorruptSchema reports a schema blob that fails magic, version,
	// length, or checksum validation.
	ErrCorruptSchema = errors.New("schema: corrupt definitions")
)

// NamespaceDef is one schema entry: its unique number, unique name, owning
// index kind, entity kind, and kind-specific definition bytes. Config is opaque
// to this package and is marshaled by the index package that owns the kind.
//
// Entity is zero (EntityInvalid) only for schemas written before entity
// support existed; the unified index Store rejects such namespaces. Every
// namespace has exactly one entity kind, so the same numeric ValueID under
// different entities is independent.
type NamespaceDef struct {
	Namespace Namespace
	Name      string
	Kind      NamespaceKind
	Entity    EntityKind
	Config    []byte
}

// Definitions is an ordered set of unique namespace definitions. The zero value
// is a valid empty schema. It is not safe for concurrent mutation: register
// namespaces during setup before sharing the schema. Config is copied both
// when registered and when returned, so callers cannot mutate its contents.
type Definitions struct {
	defs   []NamespaceDef
	byID   map[Namespace]int
	byName map[string]int
}

// NewDefinitions returns an empty set of namespace definitions.
func NewDefinitions() *Definitions { return &Definitions{} }

// Register adds one namespace definition. It rejects the zero namespace,
// empty or duplicate names, unknown kinds, and duplicate namespace numbers.
func (s *Definitions) Register(def NamespaceDef) error {
	if !def.Namespace.Valid() {
		return ErrNamespaceInvalid
	}
	if def.Name == "" || !utf8.ValidString(def.Name) || len(def.Name) > math.MaxUint16 {
		return fmt.Errorf("%w: namespace %d name %q", ErrNamespaceName, def.Namespace, def.Name)
	}
	if !def.Kind.Valid() {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceKind, def.Namespace)
	}
	if def.Entity != EntityInvalid && !def.Entity.Valid() {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceEntity, def.Namespace)
	}
	if s.byID == nil {
		s.byID = make(map[Namespace]int)
		s.byName = make(map[string]int)
	}
	if _, ok := s.byID[def.Namespace]; ok {
		return fmt.Errorf("%w: namespace %d name %q", ErrNamespaceDuplicate, def.Namespace, def.Name)
	}
	if prior, ok := s.byName[def.Name]; ok {
		return fmt.Errorf("%w: namespace %d name %q already used by namespace %d", ErrNamespaceName, def.Namespace, def.Name, s.defs[prior].Namespace)
	}
	def.Config = append([]byte(nil), def.Config...)
	s.byID[def.Namespace] = len(s.defs)
	s.byName[def.Name] = len(s.defs)
	s.defs = append(s.defs, def)
	return nil
}

// RegisterAll registers defs in order, stopping at the first failure. A
// partial registration is left in place; callers building a fresh schema can
// discard it on error.
func (s *Definitions) RegisterAll(defs ...NamespaceDef) error {
	for _, def := range defs {
		if err := s.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// Lookup returns the definition for a namespace number.
func (s *Definitions) Lookup(ns Namespace) (NamespaceDef, bool) {
	if s.byID == nil {
		return NamespaceDef{}, false
	}
	index, ok := s.byID[ns]
	if !ok {
		return NamespaceDef{}, false
	}
	return cloneDef(s.defs[index]), true
}

// LookupIdentity returns only immutable scalar metadata, without copying
// Config. Use Lookup when kind-specific configuration is needed.
func (s *Definitions) LookupIdentity(ns Namespace) (NamespaceDef, bool) {
	index, ok := s.byID[ns]
	if !ok {
		return NamespaceDef{}, false
	}
	def := s.defs[index]
	def.Config = nil
	return def, true
}

// LookupName returns the definition for a namespace name.
func (s *Definitions) LookupName(name string) (NamespaceDef, bool) {
	if s.byName == nil {
		return NamespaceDef{}, false
	}
	index, ok := s.byName[name]
	if !ok {
		return NamespaceDef{}, false
	}
	return cloneDef(s.defs[index]), true
}

func cloneDef(def NamespaceDef) NamespaceDef {
	def.Config = append([]byte(nil), def.Config...)
	return def
}

// Len returns the number of registered namespaces.
func (s *Definitions) Len() int { return len(s.defs) }

// Defs returns a copy of the registered definitions in registration order.
func (s *Definitions) Defs() []NamespaceDef {
	out := make([]NamespaceDef, len(s.defs))
	for i, def := range s.defs {
		out[i] = cloneDef(def)
	}
	return out
}

// DefsOfKind returns a copy of the definitions with the given kind, in
// registration order.
func (s *Definitions) DefsOfKind(kind NamespaceKind) []NamespaceDef {
	var out []NamespaceDef
	for _, def := range s.defs {
		if def.Kind == kind {
			out = append(out, cloneDef(def))
		}
	}
	return out
}

// DefsOfEntity returns a copy of the definitions with the given entity kind,
// in registration order.
func (s *Definitions) DefsOfEntity(entity EntityKind) []NamespaceDef {
	var out []NamespaceDef
	for _, def := range s.defs {
		if def.Entity == entity {
			out = append(out, cloneDef(def))
		}
	}
	return out
}

const (
	schemaMagic         uint32 = 0x4e535043 // NSPC
	schemaVersion       uint16 = 2          // current: per-definition EntityKind
	schemaVersionLegacy uint16 = 1          // pre-entity: Entity is EntityInvalid
)

// MarshalBinary encodes the schema deterministically in registration order:
// magic, version, count, then per definition namespace, kind, entity kind, name
// length, config length, name, config, and a trailing CRC32 over the preceding
// bytes. The per-definition layout is unchanged from version 1 — the byte that
// was reserved padding now carries EntityKind — so the encoding stays a fixed
// 10-byte header plus name and config bytes either way.
func (s *Definitions) MarshalBinary() ([]byte, error) {
	size := 4 + 2 + 4 + 4 // magic, version, count, crc
	if uint64(len(s.defs)) > math.MaxUint32 {
		return nil, ErrCorruptSchema
	}
	for _, def := range s.defs {
		if len(def.Name) > math.MaxUint16 || uint64(len(def.Config)) > math.MaxUint32 ||
			len(def.Name) > math.MaxInt-size-10 || len(def.Config) > math.MaxInt-size-10-len(def.Name) {
			return nil, ErrCorruptSchema
		}
		size += 2 + 1 + 1 + 2 + 4 + len(def.Name) + len(def.Config)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint32(out, schemaMagic)
	out = binary.LittleEndian.AppendUint16(out, schemaVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(s.defs)))
	for _, def := range s.defs {
		out = binary.LittleEndian.AppendUint16(out, uint16(def.Namespace))
		out = append(out, byte(def.Kind), byte(def.Entity))
		out = binary.LittleEndian.AppendUint16(out, uint16(len(def.Name)))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(def.Config)))
		out = append(out, def.Name...)
		out = append(out, def.Config...)
	}
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out)), nil
}

// UnmarshalBinary replaces the schema from MarshalBinary output. It rejects
// trailing bytes, unknown versions, invalid definitions, and duplicates, and
// leaves the receiver unchanged on failure.
func (s *Definitions) UnmarshalBinary(data []byte) error {
	if len(data) < 14 {
		return ErrCorruptSchema
	}
	body, trailer := data[:len(data)-4], data[len(data)-4:]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(trailer) {
		return ErrCorruptSchema
	}
	if binary.LittleEndian.Uint32(body[0:4]) != schemaMagic {
		return ErrCorruptSchema
	}
	version := binary.LittleEndian.Uint16(body[4:6])
	if version != schemaVersion && version != schemaVersionLegacy {
		return ErrCorruptSchema
	}
	count := binary.LittleEndian.Uint32(body[6:10])
	if uint64(count) > uint64(len(body)-10)/10 {
		return ErrCorruptSchema
	}
	next := &Definitions{}
	offset := 10
	for range count {
		if offset+10 > len(body) {
			return ErrCorruptSchema
		}
		def := NamespaceDef{
			Namespace: Namespace(binary.LittleEndian.Uint16(body[offset : offset+2])),
			Kind:      NamespaceKind(body[offset+2]),
		}
		// Version 1 reserved this byte as zero padding; version 2 stores the
		// entity kind there. Version-1 schemas therefore yield EntityInvalid,
		// never a silently fabricated entity.
		if version >= schemaVersion {
			def.Entity = EntityKind(body[offset+3])
		} else if body[offset+3] != 0 {
			return ErrCorruptSchema
		}
		nameLen := int(binary.LittleEndian.Uint16(body[offset+4 : offset+6]))
		configLen := int(binary.LittleEndian.Uint32(body[offset+6 : offset+10]))
		offset += 10
		if nameLen > len(body)-offset || configLen > len(body)-offset-nameLen {
			return ErrCorruptSchema
		}
		def.Name = string(body[offset : offset+nameLen])
		offset += nameLen
		def.Config = append([]byte(nil), body[offset:offset+configLen]...)
		offset += configLen
		if err := next.Register(def); err != nil {
			return err
		}
	}
	if offset != len(body) {
		return ErrCorruptSchema
	}
	*s = *next
	return nil
}
