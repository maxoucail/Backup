// Package backupjob orchestrates one backup run: scan the configured
// folders, skip files that provably haven't changed since the last run,
// hash and chunk the rest, upload only the chunks the server doesn't
// already have (globally deduplicated, not just against this device's own
// history), and submit the resulting manifest.
package backupjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"backup-agent/internal/client"
	"backup-agent/internal/config"
	"backup-agent/internal/hasher"
	"backup-agent/internal/protocol"
	"backup-agent/internal/scanner"
)

// uploadConcurrency bounds how many chunk uploads run at once for a single
// backup. High enough for real throughput on a LAN, low enough that one
// device backing up doesn't monopolize the server or the agent's own
// upstream bandwidth - multiple devices backing up at the same time each
// get their own such budget, handled independently by the server.
const uploadConcurrency = 4

const checkChunksBatchSize = 500

type Progress struct {
	Phase string // scanning / hashing / uploading / finalizing
	// SnapshotID is empty during "scanning", before CreateSnapshot has run -
	// there's no row yet for a caller to attach this progress to.
	SnapshotID    string
	FileCount     int
	LogicalBytes  int64
	UploadedBytes int64
	Percent       float64
	EtaSeconds    int64
}

type ProgressFunc func(Progress)

type Result struct {
	SnapshotID    string
	FileCount     int
	LogicalBytes  int64
	UploadedBytes int64
	// SkippedFiles are files left out of this snapshot because their data
	// couldn't be read or uploaded (typically edited or locked while the
	// backup ran). The rest of the snapshot is complete and restorable.
	SkippedFiles []string
}

func Run(ctx context.Context, c *client.Client, kind string, roots []string, chunkSize int64, onProgress ProgressFunc) (*Result, error) {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}

	onProgress(Progress{Phase: "scanning"})
	files := scanner.Walk(roots)

	cache := loadCache()
	cacheIndex := make(map[string]protocol.ManifestFile, len(cache.Files))
	for _, f := range cache.Files {
		cacheIndex[f.Path] = f
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

	onProgress(Progress{Phase: "hashing", SnapshotID: snapshotID, FileCount: len(files)})

	manifest := &protocol.Manifest{Files: make([]protocol.ManifestFile, 0, len(files))}
	var logicalBytes int64
	type uploadTask struct {
		path  string
		index int
		hash  string
		size  int64
	}
	var tasks []uploadTask
	seenHash := make(map[string]bool)

	for i, fe := range files {
		if ctx.Err() != nil {
			return finishFailed(ctx, c, snapshotID, "annulé", protocol.SnapshotStatusCancelled)
		}
		logicalBytes += fe.Size

		if cached, ok := cacheIndex[fe.RelPath]; ok && cached.Size == fe.Size && cached.ModTime == fe.ModTime {
			manifest.Files = append(manifest.Files, cached)
			continue
		}

		res, err := hasher.HashAndChunk(fe.AbsPath, chunkSize)
		if err != nil {
			log.Printf("backup: impossible de lire %s: %v", fe.AbsPath, err)
			continue
		}
		mf := protocol.ManifestFile{Path: fe.RelPath, Size: fe.Size, ModTime: fe.ModTime, SHA256: res.SHA256, Chunks: res.Chunks}
		manifest.Files = append(manifest.Files, mf)

		for idx, h := range res.Chunks {
			if seenHash[h] {
				continue
			}
			seenHash[h] = true
			size := chunkSize
			if idx == len(res.Chunks)-1 {
				size = fe.Size - int64(idx)*chunkSize
			}
			tasks = append(tasks, uploadTask{path: fe.AbsPath, index: idx, hash: h, size: size})
		}

		if i%50 == 0 {
			onProgress(Progress{Phase: "hashing", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes})
		}
	}

	// Ask the server which of the candidate chunks it doesn't already have
	// - across every device, not just this one's history - and drop the
	// rest from the upload list before touching the network again.
	if len(tasks) > 0 {
		hashes := make([]string, len(tasks))
		for i, t := range tasks {
			hashes[i] = t.hash
		}
		missing := make(map[string]bool, len(hashes))
		for start := 0; start < len(hashes); start += checkChunksBatchSize {
			end := min(start+checkChunksBatchSize, len(hashes))
			m, err := c.CheckChunks(ctx, snapshotID, hashes[start:end])
			if err != nil {
				return finishFailed(ctx, c, snapshotID, "vérification des chunks: "+err.Error(), protocol.SnapshotStatusFailed)
			}
			for _, h := range m {
				missing[h] = true
			}
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			if missing[t.hash] {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	var totalToUpload int64
	for _, t := range tasks {
		totalToUpload += t.size
	}

	var uploadedBytes int64
	var uploadedMu sync.Mutex
	start := time.Now()

	// A backup runs against a live machine: files get edited, locked or
	// deleted while it works (a mail client's data file, a document the
	// user saves, a browser profile). Those produce per-chunk failures -
	// a read error, or a hash the server rejects because the bytes changed
	// since we hashed them. Failing the entire run for one such file would
	// mean routinely losing the backup of everything else, so failures are
	// collected per chunk and only the affected files are dropped from the
	// manifest. fatalErr is reserved for problems that make continuing
	// pointless (credentials revoked), where finishing is the wrong answer.
	failedChunks := make(map[string]struct{})
	var skippedFiles []string
	var fatalErr error
	var errMu sync.Mutex

	if len(tasks) > 0 {
		onProgress(Progress{Phase: "uploading", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes})

		taskCh := make(chan uploadTask)
		var wg sync.WaitGroup
		for w := 0; w < uploadConcurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range taskCh {
					r, err := hasher.ChunkReader(t.path, t.index, chunkSize)
					if err != nil {
						noteChunkFailure(&errMu, failedChunks, &fatalErr, t.hash, t.path, err)
						continue
					}
					err = c.UploadChunk(ctx, t.hash, r, t.size)
					r.Close()
					if err != nil {
						noteChunkFailure(&errMu, failedChunks, &fatalErr, t.hash, t.path, err)
						continue
					}
					uploadedMu.Lock()
					uploadedBytes += t.size
					done := uploadedBytes
					uploadedMu.Unlock()

					elapsed := time.Since(start).Seconds()
					var eta int64
					if elapsed > 0.5 && done > 0 {
						rate := float64(done) / elapsed
						if rate > 0 {
							eta = int64(float64(totalToUpload-done) / rate)
						}
					}
					pct := 100.0
					if totalToUpload > 0 {
						pct = 100 * float64(done) / float64(totalToUpload)
					}
					onProgress(Progress{
						Phase: "uploading", SnapshotID: snapshotID, FileCount: len(files), LogicalBytes: logicalBytes,
						UploadedBytes: done, Percent: pct, EtaSeconds: eta,
					})
				}
			}()
		}
	loop:
		for _, t := range tasks {
			select {
			case taskCh <- t:
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
	}

	// Drop files whose data didn't make it, keeping everything that did.
	if len(failedChunks) > 0 {
		kept := manifest.Files[:0]
		for _, f := range manifest.Files {
			if referencesFailedChunk(f, failedChunks) {
				skippedFiles = append(skippedFiles, f.Path)
				continue
			}
			kept = append(kept, f)
		}
		manifest.Files = kept
		log.Printf("backup: %d fichier(s) ignoré(s) car modifiés ou illisibles pendant la sauvegarde", len(skippedFiles))

		if len(manifest.Files) == 0 {
			return finishFailed(ctx, c, snapshotID,
				"aucun fichier n'a pu être sauvegardé", protocol.SnapshotStatusFailed)
		}
	}

	onProgress(Progress{Phase: "finalizing", SnapshotID: snapshotID, FileCount: len(manifest.Files), LogicalBytes: logicalBytes, UploadedBytes: uploadedBytes, Percent: 100})

	manifest.SnapshotID = snapshotID
	if err := c.SubmitManifest(ctx, snapshotID, manifest); err != nil {
		return finishFailed(ctx, c, snapshotID, "envoi du manifeste: "+err.Error(), protocol.SnapshotStatusFailed)
	}
	if err := c.FinishSnapshot(ctx, snapshotID, protocol.SnapshotStatusSuccess, "", uploadedBytes); err != nil {
		return nil, fmt.Errorf("finalisation: %w", err)
	}

	saveCache(manifest)

	return &Result{
		SnapshotID: snapshotID, FileCount: len(manifest.Files),
		LogicalBytes: logicalBytes, UploadedBytes: uploadedBytes, SkippedFiles: skippedFiles,
	}, nil
}

// noteChunkFailure records that one chunk couldn't be uploaded. An
// authentication failure is escalated to fatal: the device has been
// decommissioned server-side, so every remaining chunk would fail too and
// the agent needs to stop and re-enroll rather than grind through the
// whole file list.
func noteChunkFailure(mu *sync.Mutex, failed map[string]struct{}, fatal *error, hash, path string, err error) {
	mu.Lock()
	defer mu.Unlock()
	failed[hash] = struct{}{}
	if errors.Is(err, client.ErrUnauthorized) && *fatal == nil {
		*fatal = err
		return
	}
	log.Printf("backup: chunk ignoré pour %s: %v", path, err)
}

func referencesFailedChunk(f protocol.ManifestFile, failed map[string]struct{}) bool {
	for _, h := range f.Chunks {
		if _, bad := failed[h]; bad {
			return true
		}
	}
	return false
}

func finishFailed(ctx context.Context, c *client.Client, snapshotID, message, status string) (*Result, error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = c.FinishSnapshot(finishCtx, snapshotID, status, message, 0)
	return nil, fmt.Errorf("%s", message)
}

func loadCache() *protocol.Manifest {
	p, err := config.ManifestCachePath()
	if err != nil {
		return &protocol.Manifest{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return &protocol.Manifest{}
	}
	var m protocol.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return &protocol.Manifest{}
	}
	return &m
}

func saveCache(m *protocol.Manifest) {
	p, err := config.ManifestCachePath()
	if err != nil {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(p, data, 0o600)
}
