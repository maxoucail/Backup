// Package scheduler runs the server's background maintenance: rotating
// snapshot retention (deleting the oldest backups past the configured
// limit and reclaiming their now-unreferenced chunks), purging old event
// log rows so the database stays small, marking devices offline when their
// agent stops checking in, and cleaning up expired enrollment keys.
package scheduler

import (
	"context"
	"database/sql"
	"log"
	"time"

	"backup-server/internal/models"
	"backup-server/internal/storage"
)

const staleDeviceAfter = 10 * time.Minute

// RotateRetention enforces the keep-count for one device: oldest
// successful snapshots beyond the limit are deleted (DB row + manifest),
// then the chunk store is garbage collected. Safe to call after every
// finished snapshot and periodically as a safety net.
func RotateRetention(db *sql.DB, storeHolder *storage.Holder, deviceID string) (deleted int, err error) {
	store := storeHolder.Get()
	device, err := models.GetDevice(db, deviceID)
	if err != nil {
		return 0, err
	}
	settings, err := models.GetSettings(db)
	if err != nil {
		return 0, err
	}

	keep := settings.DefaultRetentionCount
	if device.RetentionCount != nil {
		keep = *device.RetentionCount
	}
	if keep <= 0 {
		keep = 1
	}

	snaps, err := models.ListSuccessfulSnapshotsForDevice(db, deviceID)
	if err != nil {
		return 0, err
	}
	if len(snaps) <= keep {
		return 0, nil
	}

	toDelete := snaps[:len(snaps)-keep]
	for _, s := range toDelete {
		if err := storage.DeleteManifest(s.ManifestPath); err != nil {
			log.Printf("retention: could not remove manifest for snapshot %s: %v", s.ID, err)
		}
		if err := models.DeleteSnapshot(db, s.ID); err != nil {
			log.Printf("retention: could not delete snapshot row %s: %v", s.ID, err)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		remaining, err := models.AllManifestPaths(db)
		if err != nil {
			return deleted, err
		}
		freed, removed, err := store.GarbageCollect(remaining)
		if err != nil {
			log.Printf("retention: garbage collection error: %v", err)
		} else {
			log.Printf("retention: device %s rotated %d old snapshot(s), reclaimed %d chunk(s) (%d bytes)",
				deviceID, deleted, removed, freed)
		}
		msg := "Rotation de rétention : anciennes sauvegardes supprimées, espace disque récupéré."
		_ = models.AddEvent(db, &deviceID, models.EventLevelInfo, msg)
	}
	return deleted, nil
}

// Run blocks, performing periodic maintenance until ctx is cancelled.
func Run(ctx context.Context, db *sql.DB, store *storage.Holder) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	sweepEvery := 10
	tick := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick++

			if n, err := models.MarkStaleDevicesOffline(db, staleDeviceAfter); err != nil {
				log.Printf("scheduler: mark stale devices: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: marked %d device(s) offline (no check-in)", n)
			}

			if n, err := models.PurgeExpiredEnrollmentKeys(db); err != nil {
				log.Printf("scheduler: purge enrollment keys: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: purged %d expired enrollment key(s)", n)
			}

			settings, err := models.GetSettings(db)
			if err != nil {
				log.Printf("scheduler: load settings: %v", err)
				continue
			}
			if n, err := models.PurgeOldEvents(db,
				time.Duration(settings.EventRetentionDays)*24*time.Hour,
				settings.EventRetentionMaxRows); err != nil {
				log.Printf("scheduler: purge events: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: purged %d old event row(s)", n)
			}

			if tick%sweepEvery == 0 {
				devices, err := models.ListDevices(db)
				if err != nil {
					log.Printf("scheduler: list devices for retention sweep: %v", err)
					continue
				}
				for _, d := range devices {
					if _, err := RotateRetention(db, store, d.ID); err != nil {
						log.Printf("scheduler: retention sweep for %s: %v", d.ID, err)
					}
				}
			}
		}
	}
}
