// Package backupjob orchestrates one backup run: scan the configured
// folders, announce to the server everything this machine currently holds,
// and upload only the files the server replies that it needs.
//
// There is no chunking, no hashing and no manifest. The server keeps each
// machine's files as an ordinary folder tree on the NAS, so "what do you
// already have" is answered by comparing size and modification time
// against the files sitting there - which is both cheaper than hashing and
// exactly what makes the result restorable by hand, with nothing but a
// file explorer.
package backupjob

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"backup-agent/internal/client"
	"backup-agent/internal/protocol"
	"backup-agent/internal/scanner"
)

// uploadConcurrency bounds how many file uploads run at once for a single
// backup. High enough for real throughput on a LAN, low enough that one
// device backing up doesn't monopolize the server or the agent's own
// upstream bandwidth - multiple devices backing up at the same time each
// get their own such budget, handled independently by the server.
//
// Briefly raised to 8 to hide per-request latency behind more uploads in
// flight at once, on the reasoning that most of a real backup's file
// count is small files where round-trip time, not link bandwidth, caps
// throughput. Reverted: a device reachable only through a routed/VPN path
// rather than a flat LAN (a different subnet than the server's) started
// dropping several of those simultaneous new connections at once right at
// the start of the upload phase - a middlebox on that path (NAT,
// stateful firewall, VPN concentrator) apparently doesn't tolerate 8 at
// once the way a plain switch does. 4 is the value this was tested against
// for the rest of the project.
const uploadConcurrency = 4

type Progress struct {
	Phase string // scanning / planning / uploading / finalizing
	// SnapshotID is empty during "scanning", before CreateSnapshot has run -
	// there's no row yet for a caller to attach this progress to.
	SnapshotID    string
	FileCount     int
	LogicalBytes  int64
	UploadedBytes int64
	Percent       float64
	EtaSeconds    int64
	// BytesPerSec is the same rolling-window measurement EtaSeconds is
	// derived from - see etaCheckpoint.
	BytesPerSec float64
}

// etaWindow bounds how far back an etaCheckpoint looks to estimate the
// current transfer rate.
const etaWindow = 3 * time.Second

// etaCheckpoint tracks the (time, bytes) of the last rate sample, so the
// reported ETA reflects a recent window rather than the average since the
// upload started. A cumulative average is dragged down for a long time by
// the very first sample, which is dominated by fixed per-request overhead
// (connection setup, the server's own bookkeeping for that file) rather
// than real throughput - one small file taking, say, 200ms looks like a
// few KB/s, and against a total of several GB that turns into an estimate
// of hundreds of hours even on a fast LAN. Not safe for concurrent use;
// the caller (Run's upload loop) already serializes access to it under
// uploadedMu alongside the byte counter it derives its rate from.
type etaCheckpoint struct {
	at       time.Time
	bytes    int64
	lastETA  int64
	lastRate float64 // bytes/sec, from the same window as lastETA
}

// update folds in a new (done, total) sample taken at now, rolling the
// checkpoint forward and recomputing the ETA and current rate once at
// least etaWindow has passed since the last one - otherwise it returns the
// values from the last completed window, so the display doesn't reset to
// nothing between windows or jitter on every single small file.
func (c *etaCheckpoint) update(now time.Time, done, total int64) (etaSeconds int64, bytesPerSec float64) {
	if since := now.Sub(c.at); since >= etaWindow {
		if rate := float64(done-c.bytes) / since.Seconds(); rate > 0 {
			c.lastRate = rate
			if done < total {
				c.lastETA = int64(float64(total-done) / rate)
			} else {
				c.lastETA = 0
			}
		}
		c.at, c.bytes = now, done
	}
	return c.lastETA, c.lastRate
}

// ErrPermissionDenied wraps the error Run returns when a majority of the
// files it needed to send were refused outright by the OS (as opposed to
// missing, edited mid-run, or any other per-file hiccup) - the shape a
// revoked macOS Full Disk Access grant produces. Callers can check for it
// with errors.Is to put something more actionable on screen than a
// generic "backup failed".
var ErrPermissionDenied = errors.New("accès refusé pour la plupart des fichiers")

type ProgressFunc func(Progress)

type Result struct {
	SnapshotID   string
	FileCount    int
	LogicalBytes int64
	// UploadedBytes is what actually crossed the network this run, which
	// on a steady-state machine is a small fraction of LogicalBytes.
	UploadedBytes int64
	// SkippedFiles are files left out of this run because they couldn't be
	// read or sent (typically edited or locked while the backup ran).
	// Everything else still landed, and the copy of a skipped file from
	// the previous run is still on the NAS.
	SkippedFiles []string
}

func Run(ctx context.Context, c *client.Client, kind string, roots []scanner.Root, onProgress ProgressFunc) (*Result, error) {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}

	onProgress(Progress{Phase: "scanning"})
	files := scanner.Walk(roots)
	if len(files) == 0 {
		return nil, fmt.Errorf("aucun fichier à sauvegarder dans les dossiers configurés")
	}

	announced := make([]protocol.FileInfo, 0, len(files))
	absByRel := make(map[string]string, len(files))
	var logicalBytes int64
	for _, fe := range files {
		announced = append(announced, protocol.FileInfo{Path: fe.RelPath, Size: fe.Size, ModTime: fe.ModTime})
		absByRel[fe.RelPath] = fe.AbsPath
		logicalBytes += fe.Size
	}

	snapshotID, err := c.CreateSnapshot(ctx, kind)
	if err != nil {
		// Queued behind another machine: not a failure, and deliberately
		// no snapshot row exists yet, so there's nothing to mark failed.
		// The server will send a start command when a slot frees up.
		if errors.Is(err, client.ErrQueued) {
			return nil, err
		}
		return nil, fmt.Errorf("création de la sauvegarde: %w", err)
	}

	onProgress(Progress{Phase: "planning", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes})

	needed, destination, err := c.Plan(ctx, snapshotID, announced)
	if err != nil {
		return finishFailed(ctx, c, snapshotID, "préparation de la sauvegarde: "+err.Error(), protocol.SnapshotStatusFailed)
	}
	if destination != "" {
		log.Printf("backup: destination sur le serveur: %s", destination)
	}
	log.Printf("backup: %d fichier(s) au total, %d à envoyer", len(files), len(needed))

	if ctx.Err() != nil {
		return finishFailed(ctx, c, snapshotID, "annulé", protocol.SnapshotStatusCancelled)
	}

	// Nothing changed since the last run: the machine's folder on the NAS
	// is already an exact copy, and the server has just preserved the
	// previous state as a new version. A completely idle machine still
	// gets a successful backup out of this - correctly, because its backup
	// is in fact up to date.
	if len(needed) == 0 {
		onProgress(Progress{Phase: "finalizing", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes, Percent: 100})
		if err := c.FinishSnapshot(ctx, snapshotID, protocol.SnapshotStatusSuccess, "", 0); err != nil {
			return nil, fmt.Errorf("finalisation: %w", err)
		}
		return &Result{SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes}, nil
	}

	var totalToUpload int64
	sizeByRel := make(map[string]int64, len(announced))
	for _, f := range announced {
		sizeByRel[f.Path] = f.Size
	}
	for _, rel := range needed {
		totalToUpload += sizeByRel[rel]
	}

	onProgress(Progress{Phase: "uploading", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes})

	var (
		uploadedBytes    int64
		uploadedMu       sync.Mutex
		skippedFiles     []string
		permissionDenied int
		fatalErr         error
		errMu            sync.Mutex
	)
	checkpoint := etaCheckpoint{at: time.Now()}

	taskCh := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < uploadConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range taskCh {
				n, err := uploadOne(ctx, c, rel, absByRel[rel])
				if err != nil {
					noteFileFailure(&errMu, &skippedFiles, &permissionDenied, &fatalErr, rel, err)
					continue
				}
				uploadedMu.Lock()
				uploadedBytes += n
				done := uploadedBytes
				eta, rate := checkpoint.update(time.Now(), done, totalToUpload)
				uploadedMu.Unlock()

				pct := 100.0
				if totalToUpload > 0 {
					pct = 100 * float64(done) / float64(totalToUpload)
					if pct > 100 {
						pct = 100
					}
				}
				onProgress(Progress{
					Phase: "uploading", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes,
					UploadedBytes: done, Percent: pct, EtaSeconds: eta, BytesPerSec: rate,
				})
			}
		}()
	}
loop:
	for _, rel := range needed {
		select {
		case taskCh <- rel:
		case <-ctx.Done():
			break loop
		}
	}
	close(taskCh)
	wg.Wait()

	if ctx.Err() != nil {
		return finishFailed(ctx, c, snapshotID, "annulé", protocol.SnapshotStatusCancelled)
	}
	if fatalErr != nil {
		return finishFailed(ctx, c, snapshotID, fatalErr.Error(), protocol.SnapshotStatusFailed)
	}
	if len(skippedFiles) > 0 {
		log.Printf("backup: %d fichier(s) ignoré(s) car modifiés ou illisibles pendant la sauvegarde", len(skippedFiles))
	}
	// Every single file failing is not "a few locked files": something is
	// wrong with the run as a whole, and calling it a success would tell
	// an operator their machine is protected when nothing was written.
	if len(skippedFiles) == len(needed) {
		return finishFailed(ctx, c, snapshotID,
			"aucun fichier n'a pu être envoyé", protocol.SnapshotStatusFailed)
	}
	// A majority of files rejected by the OS itself (not "modified or
	// locked" - actually refused) is the exact shape of macOS revoking
	// Full Disk Access: directory listing still works (that's how the
	// scan phase found these files and their sizes at all), but opening
	// their contents doesn't, across an entire protected folder. A
	// handful of files uploading fine from outside those folders would
	// otherwise let this land as a "successful" backup that actually
	// protected almost nothing - the exact silent failure that erodes
	// trust in the whole system. ErrPermissionDenied lets the caller put
	// a specific, actionable notification on screen instead of a generic
	// failure log line.
	//
	// Deliberately scoped to this one cause (os.IsPermission), not to
	// "most files failed" in general: a server restart, a network drop,
	// or anything else severing in-flight uploads mid-run can just as
	// easily fail most of a backup, and that is not a reason to escalate
	// to a hard failure - it's an expected, ordinary side effect of taking
	// the server down while a backup happens to be running, not a symptom
	// of anything broken on this machine.
	if isDiskAccessDenied(permissionDenied, len(needed)) {
		msg := fmt.Sprintf("%d des %d fichiers à envoyer ont été refusés par le système d'exploitation "+
			"(pas modifiés ou verrouillés : un vrai refus d'accès). Sur macOS, autorisez backup-agent dans "+
			"Réglages Système -> Confidentialité et sécurité -> Accès complet au disque, puis relancez une sauvegarde.",
			permissionDenied, len(needed))
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = c.FinishSnapshot(finishCtx, snapshotID, protocol.SnapshotStatusFailed, msg, 0)
		cancel()
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, msg)
	}

	onProgress(Progress{Phase: "finalizing", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes, UploadedBytes: uploadedBytes, Percent: 100})

	if err := c.FinishSnapshot(ctx, snapshotID, protocol.SnapshotStatusSuccess, "", uploadedBytes); err != nil {
		return nil, fmt.Errorf("finalisation: %w", err)
	}

	return &Result{
		SnapshotID: snapshotID, FileCount: len(files),
		LogicalBytes: logicalBytes, UploadedBytes: uploadedBytes, SkippedFiles: skippedFiles,
	}, nil
}

// uploadOne sends a single file. Size and modification time are re-read
// here rather than reused from the scan: on a machine someone is using,
// minutes can pass between the two, and what gets stored on the NAS has to
// be described by the timestamp stored alongside it - otherwise the next
// run's comparison is against a file the server never actually holds, and
// it re-uploads forever. The full nanosecond precision is sent, not just
// the second: that precision is what lets the server tell apart a real
// edit from one that lands in the same second as a previous read.
func uploadOne(ctx context.Context, c *client.Client, relPath, absPath string) (int64, error) {
	if absPath == "" {
		return 0, fmt.Errorf("chemin local inconnu")
	}
	f, err := os.Open(absPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("n'est pas un fichier ordinaire")
	}
	size := info.Size()
	if err := c.UploadFile(ctx, relPath, info.ModTime().UnixNano(), size, f); err != nil {
		return 0, err
	}
	return size, nil
}

// noteFileFailure records that one file couldn't be sent. An
// authentication failure is escalated to fatal: the device has been
// decommissioned server-side, so every remaining file would fail too and
// the agent needs to stop and re-enroll rather than grind through the
// whole list. permissionDenied separately counts read failures the OS
// itself refused (os.IsPermission) - see the check after the upload loop
// in Run, which turns "most files hit this" into a clear failure instead
// of a quiet, mostly-empty "success".
func noteFileFailure(mu *sync.Mutex, skipped *[]string, permissionDenied *int, fatal *error, relPath string, err error) {
	mu.Lock()
	defer mu.Unlock()
	*skipped = append(*skipped, relPath)
	if os.IsPermission(err) {
		*permissionDenied++
	}
	if errors.Is(err, client.ErrUnauthorized) && *fatal == nil {
		*fatal = err
		return
	}
	log.Printf("backup: fichier ignoré %s: %v", relPath, err)
}

// isDiskAccessDenied distinguishes "most of what we needed to send was
// outright refused by the OS" from routine noise (a locked file, one edited
// mid-run): a lone permission error is unremarkable, but a majority is the
// signature of a revoked macOS Full Disk Access grant, not chance.
func isDiskAccessDenied(permissionDenied, needed int) bool {
	return permissionDenied > 0 && permissionDenied*2 >= needed
}

func finishFailed(ctx context.Context, c *client.Client, snapshotID, message, status string) (*Result, error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = c.FinishSnapshot(finishCtx, snapshotID, status, message, 0)
	return nil, fmt.Errorf("%s", message)
}
