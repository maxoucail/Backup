// Package scheduler runs the server's background maintenance: rotating
// retention (deleting a machine's oldest previous versions past the
// configured limit), purging old event log rows so the database stays
// small, marking devices offline when their agent stops checking in,
// cleaning up expired enrollment keys, and periodically recomputing total
// storage usage.
package scheduler

import (
	"context"
	"database/sql"
	"log"
	"time"

	"backup-server/internal/filestore"
	"backup-server/internal/models"
	"backup-server/internal/queue"
)

const staleDeviceAfter = 10 * time.Minute

// RotateRetention enforces the keep-count for one device by deleting its
// oldest *previous versions* on the NAS.
//
// The machine's current backup is the top-level folder and is never
// touched here; retention only ever trims _anciennes_versions. Because it
// runs after a new backup completes (see handleAgentFinishSnapshot),
// there is never a moment where the oldest version has been dropped while
// the newest is still being written - a retention of 2 always means at
// least two usable states on disk.
func RotateRetention(db *sql.DB, storeHolder *filestore.Holder, deviceID string) (deleted int, err error) {
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
	if keep < minRetention {
		keep = minRetention
	}
	// keep counts total states; the live mirror is one of them, so the
	// versions directory holds keep-1.
	deleted = store.Rotate(store.DeviceDir(device.ID, device.Name), keep-1)
	if deleted > 0 {
		log.Printf("rétention: appareil %s, %d ancienne(s) version(s) supprimée(s)", device.Name, deleted)
		msg := "Rotation de rétention : anciennes versions supprimées du stockage."
		_ = models.AddEvent(db, &deviceID, models.EventLevelInfo, msg)
	}
	return deleted, nil
}

// minRetention is the floor for how many states are kept. Two is the
// smallest number that is actually safe: with one, the only copy is the
// mirror being overwritten, so a file corrupted on the PC propagates to
// the NAS with nothing left to fall back on.
const minRetention = 2

// staleBackupAfter is how long a snapshot may sit in "running" before it's
// written off. A backup slot is normally released when the agent finishes
// or its connection drops, so this only catches the case where neither
// happened - a machine that lost power mid-backup, say. Generous enough
// not to cut off a genuinely slow first backup of a large disk.
const staleBackupAfter = 12 * time.Hour

// releaseStaleBackups marks abandoned snapshots as failed and frees the
// queue slots they were holding, so one dead machine can't stop every
// other device from ever backing up again.
func releaseStaleBackups(db *sql.DB, q *queue.Manager) {
	for _, deviceID := range q.ReleaseStale(staleBackupAfter) {
		log.Printf("file d'attente: créneau libéré pour l'appareil %s (sauvegarde sans nouvelles)", deviceID)
		_ = models.AddEvent(db, &deviceID, models.EventLevelWarning,
			"Sauvegarde interrompue sans réponse de la machine : créneau libéré pour les autres appareils.")
	}
	// Snapshot rows outlive the in-memory slot (a server restart clears the
	// queue but not the database), so close out anything still marked
	// running well past the deadline.
	cutoff := time.Now().Add(-staleBackupAfter).UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE snapshots SET status = 'failed', finished_at = ?, error_message = ?
		 WHERE status = 'running' AND started_at < ?`,
		time.Now().UTC().Format(time.RFC3339), "interrompue : plus de nouvelles de la machine", cutoff)
	if err != nil {
		log.Printf("scheduler: clôture des sauvegardes abandonnées: %v", err)
	}
}

// unconfirmedBackupAfter is how long a device gets to act on its turn once
// the queue hands it one, before that turn is given up as wasted. This is
// deliberately far shorter than staleBackupAfter: that timeout protects a
// backup that is genuinely running for hours, while this one catches a
// device that was simply never going to respond to begin with (asleep, or
// stuck) - there's nothing to wait hours for.
const unconfirmedBackupAfter = 3 * time.Minute

// releaseUnconfirmedBackups reclaims turns that were handed to a device but
// never acted on. Unlike releaseStaleBackups, there is no snapshot row to
// close out here: an unconfirmed device never got as far as creating one.
func releaseUnconfirmedBackups(db *sql.DB, q *queue.Manager) {
	for _, deviceID := range q.ReleaseUnconfirmed(unconfirmedBackupAfter) {
		log.Printf("file d'attente: tour de l'appareil %s non exploité, créneau réattribué", deviceID)
		_ = models.AddEvent(db, &deviceID, models.EventLevelWarning,
			"Le tour de cette machine dans la file d'attente est arrivé mais elle n'a pas démarré de sauvegarde : créneau réattribué à la suivante.")
	}
}

// storageUsageRefreshEvery is how often (in 1-minute ticks) the fleet-wide
// storage figure is recomputed - see refreshStorageUsage. A full walk of
// the NAS tree is the one genuinely expensive thing this package does, so
// it runs on its own, coarser cadence rather than every tick: the
// dashboard reads whatever this last wrote, never triggering a walk
// itself.
const storageUsageRefreshEvery = 5

// refreshStorageUsage recomputes total storage usage and records it, so
// GET /api/dashboard/storage is a plain, fast database read - never a
// filesystem walk on the request path. Logged but not fatal on failure:
// the dashboard keeps serving whatever figure it last managed to compute.
func refreshStorageUsage(db *sql.DB, store *filestore.Holder) {
	usedBytes, err := store.Get().UsedBytes()
	if err != nil {
		log.Printf("scheduler: calcul du stockage utilisé: %v", err)
		return
	}
	if err := models.UpdateStorageUsage(db, usedBytes); err != nil {
		log.Printf("scheduler: enregistrement du stockage utilisé: %v", err)
	}
}

// refreshStorageFree recomputes free space on the storage volume. Unlike
// refreshStorageUsage this is a single statfs() call, not a walk (see
// filestore.Store.FreeBytes), so it runs on every tick rather than every
// storageUsageRefreshEvery ticks - there's no cost here worth pacing.
func refreshStorageFree(db *sql.DB, store *filestore.Holder) {
	freeBytes, err := store.Get().FreeBytes()
	if err != nil {
		log.Printf("scheduler: calcul de l'espace disponible: %v", err)
		return
	}
	if err := models.UpdateStorageFree(db, freeBytes); err != nil {
		log.Printf("scheduler: enregistrement de l'espace disponible: %v", err)
	}
}

// Run blocks, performing periodic maintenance until ctx is cancelled.
func Run(ctx context.Context, db *sql.DB, store *filestore.Holder, q *queue.Manager) {
	// Computed once up front rather than waiting for the first tick: a
	// server that just started (every deploy restarts it) would otherwise
	// show no storage figures at all for the first several minutes.
	refreshStorageUsage(db, store)
	refreshStorageFree(db, store)

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

			// Free space is a cheap statfs(), refreshed every tick (once a
			// minute); total usage is a full walk, refreshed far less often.
			refreshStorageFree(db, store)
			if tick%storageUsageRefreshEvery == 0 {
				refreshStorageUsage(db, store)
			}

			if n, err := models.MarkStaleDevicesOffline(db, staleDeviceAfter); err != nil {
				log.Printf("scheduler: mark stale devices: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: marked %d device(s) offline (no check-in)", n)
			}

			releaseStaleBackups(db, q)
			releaseUnconfirmedBackups(db, q)

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
