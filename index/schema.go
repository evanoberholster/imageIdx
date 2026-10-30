package index

import (
	"encoding/binary"
	"fmt"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

// livenessNamespace is the reserved bitmap namespace that stores entity
// liveness. Its enum values are the EntityKind names, so the same numeric
// ValueID can be live for one entity kind and dead for another without a
// per-record entity byte.
const livenessNamespace schematypes.Namespace = 0xFFF0

// livenessName is the display name of the reserved liveness namespace.
const livenessName = "__liveness__"

// VectorDef builds a namespace definition for a vectorix namespace, encoding
// the persisted dimensions, element type, metric, and optional model into
// Config. Name and entity are supplied here; the namespace number comes from
// cfg.
func VectorDef(cfg vectoridx.Config, name string, entity schematypes.EntityKind) (schematypes.NamespaceDef, error) {
	if !cfg.Namespace.Valid() || !cfg.Dimensions.Valid() {
		return schematypes.NamespaceDef{}, ErrInvalidSchema
	}
	if !entity.Valid() {
		return schematypes.NamespaceDef{}, ErrEntityRequired
	}
	if cfg.ElementType < vectoridx.ElementInt8 || cfg.ElementType > vectoridx.ElementFloat16 {
		return schematypes.NamespaceDef{}, ErrInvalidSchema
	}
	metric := cfg.Metric
	if metric == 0 {
		metric = vectoridx.MetricL2
	}
	if len(cfg.Model) > vectoridx.MaxModelBytes {
		return schematypes.NamespaceDef{}, ErrInvalidSchema
	}
	// Keep the legacy 4-byte encoding when no model is attached; extended
	// configs append a uint16 length and the UTF-8 model ID.
	configLen := 4
	if cfg.Model != "" {
		configLen = 6 + len(cfg.Model)
	}
	config := make([]byte, configLen)
	binary.LittleEndian.PutUint16(config[0:2], uint16(cfg.Dimensions))
	config[2] = byte(cfg.ElementType)
	config[3] = metric
	if cfg.Model != "" {
		binary.LittleEndian.PutUint16(config[4:6], uint16(len(cfg.Model)))
		copy(config[6:], cfg.Model)
	}
	return schematypes.NamespaceDef{
		Namespace: cfg.Namespace,
		Name:      name,
		Kind:      schematypes.KindVector,
		Entity:    entity,
		Config:    config,
	}, nil
}

// BitmapDef builds a namespace definition for a bitmapix namespace from its
// spec, attaching the entity kind.
func BitmapDef(spec bitmapidx.NamespaceSpec, name string, entity schematypes.EntityKind) (schematypes.NamespaceDef, error) {
	if !entity.Valid() {
		return schematypes.NamespaceDef{}, ErrEntityRequired
	}
	def, err := spec.NamespaceDef(name)
	if err != nil {
		return schematypes.NamespaceDef{}, err
	}
	def.Entity = entity
	return def, nil
}

// decodeVectorConfig recovers the persisted dimensions, element type, metric,
// and optional model. Legacy 4-byte configs decode with an empty model.
func decodeVectorConfig(config []byte) (vectoridx.Config, error) {
	if len(config) != 4 && len(config) < 6 {
		return vectoridx.Config{}, ErrInvalidSchema
	}
	cfg := vectoridx.Config{
		Dimensions:  vectoridx.Dimensions(binary.LittleEndian.Uint16(config[0:2])),
		ElementType: vectoridx.ElementType(config[2]),
		Metric:      config[3],
	}
	if len(config) > 4 {
		modelLen := int(binary.LittleEndian.Uint16(config[4:6]))
		if modelLen > vectoridx.MaxModelBytes || len(config) != 6+modelLen {
			return vectoridx.Config{}, ErrInvalidSchema
		}
		cfg.Model = string(config[6:])
	}
	if !cfg.Dimensions.Valid() {
		return vectoridx.Config{}, ErrInvalidSchema
	}
	return cfg, nil
}

// engineFamily classifies a namespace kind into the engine that serves it.
type engineFamily uint8

const (
	familyNone engineFamily = iota
	familyBitmap
	familyTime
	familyGeo
	familySim
	familyRadix
	familyVector
)

func familyOf(kind schematypes.NamespaceKind) engineFamily {
	switch {
	case kind.IsBitmap():
		return familyBitmap
	case kind == schematypes.KindTimestamp:
		return familyTime
	case kind == schematypes.KindGeo:
		return familyGeo
	case kind == schematypes.KindHashPerceptual:
		return familySim
	case kind == schematypes.KindHashExact:
		return familyRadix
	case kind == schematypes.KindVector:
		return familyVector
	default:
		return familyNone
	}
}

// validateSchema checks the user schema and returns its definitions sorted by
// namespace number. It rejects unsupported kinds, missing entities, and the
// reserved liveness namespace.
func validateSchema(schema *schematypes.Definitions) ([]schematypes.NamespaceDef, error) {
	if schema == nil {
		return nil, ErrSchemaRequired
	}
	if schema.Len() == 0 {
		return nil, fmt.Errorf("%w: no namespaces", ErrInvalidSchema)
	}
	defs := schema.Defs()
	for _, def := range defs {
		if def.Namespace == livenessNamespace {
			return nil, fmt.Errorf("%w: namespace %d is reserved for liveness", ErrInvalidSchema, livenessNamespace)
		}
		if !def.Entity.Valid() {
			return nil, fmt.Errorf("%w: namespace %d", ErrEntityRequired, def.Namespace)
		}
		if familyOf(def.Kind) == familyNone {
			return nil, fmt.Errorf("%w: namespace %d kind %s", ErrInvalidSchema, def.Namespace, def.Kind)
		}
		if def.Kind == schematypes.KindVector {
			if _, err := decodeVectorConfig(def.Config); err != nil {
				return nil, fmt.Errorf("%w: namespace %d vector config", ErrInvalidSchema, def.Namespace)
			}
		}
		if def.Kind.IsBitmap() {
			if _, err := bitmapidx.NamespaceSpecFromSchemaDef(def); err != nil {
				return nil, fmt.Errorf("%w: namespace %d bitmap codec (use index.BitmapDef)", ErrInvalidSchema, def.Namespace)
			}
		}
	}
	return defs, nil
}

// cloneSchema returns an independent copy so a Store never shares mutable
// schema state with its caller.
func cloneSchema(src *schematypes.Definitions) (*schematypes.Definitions, error) {
	data, err := src.MarshalBinary()
	if err != nil {
		return nil, err
	}
	clone := schematypes.NewDefinitions()
	if err := clone.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return clone, nil
}

// livenessValues caches one enum value per entity kind. Indexing is direct and
// allocation-free; index 0 is unused.
var livenessValues = func() [schematypes.EntityKindCount + 1]bitmapidx.Value {
	var values [schematypes.EntityKindCount + 1]bitmapidx.Value
	for e := schematypes.EntityImage; e.Valid(); e++ {
		values[e] = bitmapidx.EnumStringValue(e.String())
	}
	return values
}()

func livenessValue(entity schematypes.EntityKind) bitmapidx.Value { return livenessValues[entity] }
