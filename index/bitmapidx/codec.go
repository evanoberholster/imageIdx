package bitmapidx

import (
	"encoding/binary"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

const registryVersion uint16 = 1

// encodeNamespaceSpecs encodes specs deterministically by namespace number.
// The layout is the byte-identical persisted registry format: registryVersion,
// count, then each spec's namespace, kind, storage, flags, quantizer, geo
// codec, and normalizer/analyzer codecs. Names are never persisted.
func encodeNamespaceSpecs(specs []NamespaceSpec) []byte {
	sorted := slices.Clone(specs)
	slices.SortFunc(sorted, func(a, b NamespaceSpec) int { return int(a.Namespace) - int(b.Namespace) })
	size := 2 + 4
	for _, spec := range sorted {
		size += 32 + 8 + len(spec.Normalizer.Params) + 8 + len(spec.Analyzer.Params)
	}
	out := make([]byte, 0, size)
	out = binary.LittleEndian.AppendUint16(out, registryVersion)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(sorted)))
	for _, spec := range sorted {
		out = appendNamespace(out, spec)
	}
	return out
}

// decodeNamespaceSpecs decodes and validates encodeNamespaceSpecs output.
// It rejects unknown versions, trailing bytes, invalid definitions, and
// duplicate namespace numbers.
func decodeNamespaceSpecs(data []byte) ([]NamespaceSpec, error) {
	if len(data) < 6 || binary.LittleEndian.Uint16(data[0:2]) != registryVersion {
		return nil, ErrInvalidNamespace
	}
	count := binary.LittleEndian.Uint32(data[2:6])
	if uint64(count) > uint64(len(data)) {
		return nil, ErrInvalidNamespace
	}
	specs := make([]NamespaceSpec, 0, count)
	offset := 6
	for n := uint32(0); n < count; n++ {
		spec, next, err := decodeNamespace(data, offset)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
		offset = next
	}
	if offset != len(data) {
		return nil, ErrInvalidNamespace
	}
	if err := validateNamespaceSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// bitmapKindCode keeps the original bitmap wire and hash domain codes stable.
func bitmapKindCode(kind common.NamespaceKind) byte {
	switch kind {
	case common.KindBitmapEnum:
		return 1
	case common.KindBitmapBool:
		return 2
	case common.KindBitmapString:
		return 3
	case common.KindBitmapContinuous:
		return 4
	case common.KindBitmapGeo:
		return 5
	default:
		return 0
	}
}

func bitmapKindFromCode(code byte) common.NamespaceKind {
	switch code {
	case 1:
		return common.KindBitmapEnum
	case 2:
		return common.KindBitmapBool
	case 3:
		return common.KindBitmapString
	case 4:
		return common.KindBitmapContinuous
	case 5:
		return common.KindBitmapGeo
	default:
		return 0
	}
}

func appendNamespace(out []byte, namespace NamespaceSpec) []byte {
	out = binary.LittleEndian.AppendUint16(out, uint16(namespace.Namespace))
	out = append(out, bitmapKindCode(namespace.Kind), 2) // reserved legacy storage byte
	out = binary.LittleEndian.AppendUint32(out, namespace.Flags)
	out = binary.LittleEndian.AppendUint16(out, namespace.Quant.Code)
	out = binary.LittleEndian.AppendUint16(out, namespace.Quant.Version)
	out = binary.LittleEndian.AppendUint64(out, uint64(namespace.Quant.Scale))
	out = binary.LittleEndian.AppendUint64(out, uint64(namespace.Quant.Offset))
	out = binary.LittleEndian.AppendUint16(out, namespace.Geo.Version)
	out = append(out, namespace.Geo.Level, 0)
	out = appendCodec(out, namespace.Normalizer)
	out = appendCodec(out, namespace.Analyzer)
	return out
}

func appendCodec(out []byte, spec CodecSpec) []byte {
	out = binary.LittleEndian.AppendUint16(out, spec.Code)
	out = binary.LittleEndian.AppendUint16(out, spec.Version)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(spec.Params)))
	out = append(out, spec.Params...)
	return out
}

func decodeNamespace(data []byte, offset int) (NamespaceSpec, int, error) {
	if offset+32 > len(data) {
		return NamespaceSpec{}, 0, ErrInvalidNamespace
	}
	if data[offset+3] != 1 && data[offset+3] != 2 {
		return NamespaceSpec{}, 0, ErrInvalidNamespace
	}
	namespace := NamespaceSpec{
		Namespace: common.Namespace(binary.LittleEndian.Uint16(data[offset : offset+2])),
		Kind:      bitmapKindFromCode(data[offset+2]),
		Flags:     binary.LittleEndian.Uint32(data[offset+4 : offset+8]),
		Quant: Quantizer{
			Code:    binary.LittleEndian.Uint16(data[offset+8 : offset+10]),
			Version: binary.LittleEndian.Uint16(data[offset+10 : offset+12]),
			Scale:   int64(binary.LittleEndian.Uint64(data[offset+12 : offset+20])),
			Offset:  int64(binary.LittleEndian.Uint64(data[offset+20 : offset+28])),
		},
		Geo: GeoCodec{
			Version: binary.LittleEndian.Uint16(data[offset+28 : offset+30]),
			Level:   data[offset+30],
		},
	}
	normalizer, next, err := decodeCodec(data, offset+32)
	if err != nil {
		return NamespaceSpec{}, 0, err
	}
	analyzer, next, err := decodeCodec(data, next)
	if err != nil {
		return NamespaceSpec{}, 0, err
	}
	namespace.Normalizer = normalizer
	namespace.Analyzer = analyzer
	if err := validateNamespace(namespace); err != nil {
		return NamespaceSpec{}, 0, err
	}
	return namespace, next, nil
}

func decodeCodec(data []byte, offset int) (CodecSpec, int, error) {
	if offset+8 > len(data) {
		return CodecSpec{}, 0, ErrInvalidNamespace
	}
	spec := CodecSpec{
		Code:    binary.LittleEndian.Uint16(data[offset : offset+2]),
		Version: binary.LittleEndian.Uint16(data[offset+2 : offset+4]),
	}
	length := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
	if uint64(length) > uint64(len(data)-offset-8) {
		return CodecSpec{}, 0, ErrInvalidNamespace
	}
	if length != 0 {
		spec.Params = append([]byte(nil), data[offset+8:offset+8+int(length)]...)
	}
	return spec, offset + 8 + int(length), nil
}
