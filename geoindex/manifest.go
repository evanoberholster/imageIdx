package geoindex

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
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
	temporary := filepath.Join(dir, "manifest.tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err = writeFull(file, data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, filepath.Join(dir, "manifest")); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
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
