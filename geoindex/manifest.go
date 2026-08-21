package geoindex

import (
	"encoding/binary"
	"hash/crc32"
	"os"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	geoManifestMagic   uint32 = 0x47454f4d // GEOM
	geoManifestVersion uint16 = 1
	geoManifestSize           = 40
)

type geoManifest struct {
	storageGeneration uint64
	walOffset         int64
	indexID           uint64
}

func writeGeoManifest(dir string, manifest geoManifest) error {
	data := make([]byte, geoManifestSize)
	binary.LittleEndian.PutUint32(data[0:4], geoManifestMagic)
	binary.LittleEndian.PutUint16(data[4:6], geoManifestVersion)
	binary.LittleEndian.PutUint64(data[8:16], manifest.storageGeneration)
	binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.walOffset))
	binary.LittleEndian.PutUint64(data[24:32], manifest.indexID)
	binary.LittleEndian.PutUint32(data[36:40], crc32.ChecksumIEEE(data[:36]))
	return indexutil.WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644)
}

func readGeoManifest(path string) (geoManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return geoManifest{}, err
	}
	if len(data) != geoManifestSize || binary.LittleEndian.Uint32(data[0:4]) != geoManifestMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != geoManifestVersion ||
		binary.LittleEndian.Uint32(data[36:40]) != crc32.ChecksumIEEE(data[:36]) {
		return geoManifest{}, ErrCorruptManifest
	}
	return geoManifest{
		storageGeneration: binary.LittleEndian.Uint64(data[8:16]),
		walOffset:         int64(binary.LittleEndian.Uint64(data[16:24])),
		indexID:           binary.LittleEndian.Uint64(data[24:32]),
	}, nil
}
