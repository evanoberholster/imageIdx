package geoidx

import (
	"encoding/binary"
	"os"

	"github.com/evanoberholster/imageIdx/internal/persist"
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
	data := persist.BuildManifest(geoManifestSize, geoManifestMagic, geoManifestVersion, 36, 36, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], manifest.storageGeneration)
		binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.walOffset))
		binary.LittleEndian.PutUint64(data[24:32], manifest.indexID)
	})
	return persist.WriteManifest(dir, data)
}

func readGeoManifest(path string) (geoManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return geoManifest{}, err
	}
	if err := persist.VerifyManifest(data, geoManifestSize, geoManifestMagic, geoManifestVersion, 36, 36); err != nil {
		return geoManifest{}, ErrCorruptManifest
	}
	return geoManifest{
		storageGeneration: binary.LittleEndian.Uint64(data[8:16]),
		walOffset:         int64(binary.LittleEndian.Uint64(data[16:24])),
		indexID:           binary.LittleEndian.Uint64(data[24:32]),
	}, nil
}
