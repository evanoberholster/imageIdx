package geoindex

const (
	geoStorageMagic     uint32 = 0x47454f49 // GEOI
	geoStorageVersion   uint16 = 2
	geoHeaderSize              = 128
	geoBitmapMetaSize          = 24
	geoStorageAlignment        = 4_096
)

type geoBitmapDiskMeta struct {
	offset      uint64
	length      uint32
	cardinality uint64
}
