package database

import (
	"errors"
	"fmt"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/idalloc"
	"github.com/evanoberholster/imageIdx/store"
)

// migrateIDs is the one-time legacy migration. Scan *all* canonical keys,
// including tombstones, not only live entries. Once initialized, startup
// reads only durable watermarks; missing/corrupt metadata then fails closed.
func (db *DB) migrateIDs() error {
	maxima, err := db.canonicalMaxima()
	if err != nil {
		return err
	}
	return db.values.InitializeReservations(maxima)
}

func (db *DB) canonicalMaxima() ([common.EntityKindCount + 1]uint64, error) {
	var maxima [common.EntityKindCount + 1]uint64
	for n := 1; n <= common.EntityKindCount; n++ {
		kind := common.EntityKind(n)
		if err := db.values.Scan(common.Namespace(kind), func(key store.Key, _ []byte) error {
			if key.Flags != 0 || key.ID == 0 {
				return fmt.Errorf("database: invalid canonical ID key for %s", kind)
			}
			if key.ID > maxima[n] {
				maxima[n] = key.ID
			}
			return nil
		}); err != nil {
			return maxima, err
		}
	}
	return maxima, nil
}

func (db *DB) openIDs() error {
	reservations, err := db.values.OpenReservations()
	if errors.Is(err, store.ErrUninitializedReservations) {
		if err = db.migrateIDs(); err != nil {
			return err
		}
		reservations, err = db.values.OpenReservations()
	} else if errors.Is(err, store.ErrLegacyReservations) {
		maxima, scanErr := db.canonicalMaxima()
		if scanErr != nil {
			return scanErr
		}
		if err = db.values.UpgradeReservations(maxima); err != nil {
			return err
		}
		reservations, err = db.values.OpenReservations()
	}
	if err != nil {
		return err
	}
	db.ids, err = idalloc.Open(reservations)
	return err
}
