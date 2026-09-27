package common

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

var (
	// ErrNamespaceInvalid reports the reserved zero namespace.
	ErrNamespaceInvalid = errors.New("common: namespace zero is reserved")
	// ErrNamespaceDuplicate reports a namespace number already registered.
	ErrNamespaceDuplicate = errors.New("common: duplicate namespace")
	// ErrNamespaceName reports an empty or duplicate namespace name.
	ErrNamespaceName = errors.New("common: namespace name must be non-empty and unique")
	// ErrNamespaceKind reports an unrecognized namespace kind.
	ErrNamespaceKind = errors.New("common: invalid namespace kind")
	// ErrCorruptSchema reports a schema blob that fails magic, version,
	// length, or checksum validation.
	ErrCorruptSchema = errors.New("common: corrupt schema")
)

// NamespaceDef is one schema entry: its unique number, unique name, owning
// index kind, and kind-specific definition bytes. Config is opaque to this
// package and is marshaled by the index package that owns the kind.
type NamespaceDef struct {
	Namespace Namespace
	Name      string
	Kind      NamespaceKind
	Config    []byte
}

// Schema is an ordered set of unique namespace definitions. The zero value
// is a valid empty schema. It is not safe for concurrent mutation: register
// namespaces during setup before sharing the schema. Definitions are copied
// on Register and on UnmarshalBinary, and returned slices share their Config
// bytes, which callers must treat as read-only.
type Schema struct {
	defs   []NamespaceDef
	byID   map[Namespace]int
	byName map[string]int
}

// NewSchema returns an empty schema.
func NewSchema() *Schema { return &Schema{} }

// Register adds one namespace definition. It rejects the zero namespace,
// empty or duplicate names, unknown kinds, and duplicate namespace numbers.
func (s *Schema) Register(def NamespaceDef) error {
	if !def.Namespace.Valid() {
		return ErrNamespaceInvalid
	}
	if def.Name == "" {
		return ErrNamespaceName
	}
	if !def.Kind.Valid() {
		return ErrNamespaceKind
	}
	if s.byID == nil {
		s.byID = make(map[Namespace]int)
		s.byName = make(map[string]int)
	}
	if _, ok := s.byID[def.Namespace]; ok {
		return fmt.Errorf("%w: %d", ErrNamespaceDuplicate, def.Namespace)
	}
	if _, ok := s.byName[def.Name]; ok {
		return fmt.Errorf("%w: %q", ErrNamespaceName, def.Name)
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
func (s *Schema) RegisterAll(defs ...NamespaceDef) error {
	for _, def := range defs {
		if err := s.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// Lookup returns the definition for a namespace number.
func (s *Schema) Lookup(ns Namespace) (NamespaceDef, bool) {
	if s.byID == nil {
		return NamespaceDef{}, false
	}
	index, ok := s.byID[ns]
	if !ok {
		return NamespaceDef{}, false
	}
	return s.defs[index], true
}

// LookupName returns the definition for a namespace name.
func (s *Schema) LookupName(name string) (NamespaceDef, bool) {
	if s.byName == nil {
		return NamespaceDef{}, false
	}
	index, ok := s.byName[name]
	if !ok {
		return NamespaceDef{}, false
	}
	return s.defs[index], true
}

// Len returns the number of registered namespaces.
func (s *Schema) Len() int { return len(s.defs) }

// Defs returns a copy of the registered definitions in registration order.
func (s *Schema) Defs() []NamespaceDef {
	return append([]NamespaceDef(nil), s.defs...)
}

// DefsOfKind returns a copy of the definitions with the given kind, in
// registration order.
func (s *Schema) DefsOfKind(kind NamespaceKind) []NamespaceDef {
	var out []NamespaceDef
	for _, def := range s.defs {
		if def.Kind == kind {
			out = append(out, def)
		}
	}
	return out
}

const (
	schemaMagic   uint32 = 0x4e535043 // NSPC
	schemaVersion uint16 = 1
)

// MarshalBinary encodes the schema deterministically in registration order:
// magic, version, count, then per definition namespace, kind, name length,
// config length, name, config, and a trailing CRC32 over the preceding bytes.
func (s *Schema) MarshalBinary() ([]byte, error) {
	size := 4 + 2 + 4 + 4 // magic, version, count, crc
	for _, def := range s.defs {
		size += 2 + 1 + 1 + 2 + 4 + len(def.Name) + len(def.Config)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint32(out, schemaMagic)
	out = binary.LittleEndian.AppendUint16(out, schemaVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(s.defs)))
	for _, def := range s.defs {
		out = binary.LittleEndian.AppendUint16(out, uint16(def.Namespace))
		out = append(out, byte(def.Kind), 0)
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
func (s *Schema) UnmarshalBinary(data []byte) error {
	if len(data) < 14 {
		return ErrCorruptSchema
	}
	body, trailer := data[:len(data)-4], data[len(data)-4:]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(trailer) {
		return ErrCorruptSchema
	}
	if binary.LittleEndian.Uint32(body[0:4]) != schemaMagic || binary.LittleEndian.Uint16(body[4:6]) != schemaVersion {
		return ErrCorruptSchema
	}
	count := binary.LittleEndian.Uint32(body[6:10])
	if uint64(count) > uint64(len(body)) {
		return ErrCorruptSchema
	}
	next := &Schema{}
	offset := 10
	for range count {
		if offset+10 > len(body) {
			return ErrCorruptSchema
		}
		def := NamespaceDef{
			Namespace: Namespace(binary.LittleEndian.Uint16(body[offset : offset+2])),
			Kind:      NamespaceKind(body[offset+2]),
		}
		nameLen := int(binary.LittleEndian.Uint16(body[offset+4 : offset+6]))
		configLen := int(binary.LittleEndian.Uint32(body[offset+6 : offset+10]))
		offset += 10
		if offset+nameLen+configLen > len(body) {
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
