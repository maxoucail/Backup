// Command backup-agent runs on the machine being backed up (Windows or
// macOS, though it builds and runs on Linux too). On first run it walks
// the operator through a local-web-page enrollment wizard; from then on it
// runs quietly in the background, backing up on a schedule and reacting
// to remote commands (backup now, restore) sent from the server's panel.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"backup-agent/internal/autostart"
	"backup-agent/internal/backupjob"
	"backup-agent/internal/client"
	"backup-agent/internal/config"
	"backup-agent/internal/osinfo"
	"backup-agent/internal/progressui"
	"backup-agent/internal/protocol"
	"backup-agent/internal/restorejob"
	"backup-agent/internal/scanner"
	"backup-agent/internal/setupwizard"
)

// AgentVersion is overridden at build time via -ldflags "-X main.AgentVersion=...".
var AgentVersion = "1.0.0"

const defaultIntervalMinutes = 360
const defaultRetentionCount = 7
const defaultChunkSize = 16 * 1024 * 1024

func main() {
	setupLogging()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	if !cfg.Enrolled() {
		log.Print("appareil non enrôlé, ouverture de l'assistant de configuration...")
		newCfg, err := setupwizard.Run(context.Background(), AgentVersion)
		if err != nil {
			log.Fatalf("assistant de configuration: %v", err)
		}
		cfg = newCfg
		log.Print("enrôlement réussi.")
	}

	if exePath, err := os.Executable(); err == nil {
		if err := autostart.Ensure(exePath); err != nil {
			log.Printf("avertissement: démarrage automatique non configuré: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runAgent(ctx, cfg)
}

func setupLogging() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
}

// policy is the effective, live backup policy: the operator's defaults
// pushed from the panel, refreshed both by periodic HTTP polling (belt)
// and by the WebSocket config push (suspenders - near-instant when
// connected).
type policy struct {
	mu              sync.Mutex
	intervalMinutes int
	retentionCount  int
	backupPaths     []string
	chunkSize       int64
}

func newPolicy() *policy {
	return &policy{intervalMinutes: defaultIntervalMinutes, retentionCount: defaultRetentionCount, chunkSize: defaultChunkSize}
}

func (p *policy) set(interval, retention int, paths []string, chunkSize int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if interval > 0 {
		p.intervalMinutes = interval
	}
	if retention > 0 {
		p.retentionCount = retention
	}
	p.backupPaths = paths
	if chunkSize > 0 {
		p.chunkSize = chunkSize
	}
}

func (p *policy) snapshot() (interval int, paths []string, chunkSize int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.intervalMinutes, p.backupPaths, p.chunkSize
}

func runAgent(ctx context.Context, cfg *config.Config) {
	hostname, _ := os.Hostname()
	hello := protocol.Envelope{
		Type: protocol.TypeHello, Hostname: hostname,
		OSName: runtime.GOOS, OSVersion: osinfo.Version(), AgentVersion: AgentVersion,
	}

	wsc := client.NewWSClient(cfg.ServerURL, cfg.DeviceID, cfg.DeviceSecret, hello)
	go wsc.Run(ctx)

	api := client.New(cfg.ServerURL, cfg.DeviceID, cfg.DeviceSecret)
	pol := newPolicy()

	var jobMu sync.Mutex
	var jobCancel context.CancelFunc
	jobRunning := false

	tryStartJob := func() (context.Context, func(), bool) {
		jobMu.Lock()
		defer jobMu.Unlock()
		if jobRunning {
			return nil, nil, false
		}
		jobRunning = true
		jobCtx, cancel := context.WithCancel(ctx)
		jobCancel = cancel
		done := func() {
			jobMu.Lock()
			jobRunning = false
			jobCancel = nil
			jobMu.Unlock()
		}
		return jobCtx, done, true
	}

	runBackup := func(kind string, showPopup bool) {
		jobCtx, done, ok := tryStartJob()
		if !ok {
			wsc.Send(protocol.Envelope{Type: protocol.TypeLog, Level: protocol.LevelWarning, Message: "Sauvegarde ignorée : une tâche est déjà en cours."})
			return
		}
		defer done()

		var popup *progressui.Popup
		if showPopup {
			popup, _ = progressui.Show("Sauvegarde en cours")
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupStarted})

		_, configuredPaths, chunkSize := pol.snapshot()
		roots := scanner.ResolveRoots(configuredPaths)

		result, err := backupjob.Run(jobCtx, api, kind, roots, chunkSize, func(p backupjob.Progress) {
			if popup != nil {
				popup.Update(p.Phase, p.Percent, p.EtaSeconds, p.UploadedBytes)
			}
			wsc.Send(protocol.Envelope{
				Type: protocol.TypeProgress, Phase: p.Phase, FileCount: p.FileCount,
				LogicalBytes: p.LogicalBytes, UploadedBytes: p.UploadedBytes, Percent: p.Percent, EtaSeconds: p.EtaSeconds,
			})
		})

		status, errMsg := protocol.SnapshotStatusSuccess, ""
		if err != nil {
			status, errMsg = protocol.SnapshotStatusFailed, err.Error()
			log.Printf("sauvegarde échouée: %v", err)
		} else {
			log.Printf("sauvegarde terminée: %d fichiers, %d octets envoyés", result.FileCount, result.UploadedBytes)
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupFinished, Status: status, ErrorMessage: errMsg})
		if popup != nil {
			popup.Finish(errMsg)
		}
	}

	runRestore := func(snapshotID string) {
		jobCtx, done, ok := tryStartJob()
		if !ok {
			wsc.Send(protocol.Envelope{Type: protocol.TypeLog, Level: protocol.LevelWarning, Message: "Restauration ignorée : une tâche est déjà en cours."})
			return
		}
		defer done()

		popup, _ := progressui.Show("Restauration en cours")
		wsc.Send(protocol.Envelope{Type: protocol.TypeRestoreStarted, SnapshotID: snapshotID})

		result, err := restorejob.Run(jobCtx, api, snapshotID, func(p restorejob.Progress) {
			popup.Update(p.Phase, p.Percent, p.EtaSeconds, p.RestoredBytes)
			wsc.Send(protocol.Envelope{
				Type: protocol.TypeProgress, Phase: p.Phase, FileCount: p.FileCount,
				LogicalBytes: p.LogicalBytes, UploadedBytes: p.RestoredBytes, Percent: p.Percent, EtaSeconds: p.EtaSeconds,
			})
		})

		status, errMsg := protocol.SnapshotStatusSuccess, ""
		if err != nil {
			status, errMsg = protocol.SnapshotStatusFailed, err.Error()
			log.Printf("restauration échouée: %v", err)
		} else {
			log.Printf("restauration terminée: %d fichiers restaurés", result.FileCount)
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeRestoreFinished, SnapshotID: snapshotID, Status: status, ErrorMessage: errMsg})
		popup.Finish(errMsg)
	}

	// Fallback policy refresh: the WS config push is near-instant while
	// connected, but this keeps the agent honest even if a push was
	// missed while offline.
	refreshPolicy := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		resp, err := api.GetConfig(reqCtx)
		if err != nil {
			return
		}
		pol.set(resp.IntervalMinutes, resp.RetentionCount, resp.BackupPaths, resp.ChunkSizeBytes)
	}
	refreshPolicy()
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshPolicy()
			}
		}
	}()

	// Scheduled backups, silent (no popup) - only remote/manual triggers
	// interrupt the user with a progress window.
	go func() {
		go runBackup(protocol.SnapshotKindScheduled, false)
		for {
			interval, _, _ := pol.snapshot()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(interval) * time.Minute):
				runBackup(protocol.SnapshotKindScheduled, false)
			}
		}
	}()

	log.Printf("backup-agent %s démarré (appareil %s, serveur %s)", AgentVersion, cfg.DeviceID, cfg.ServerURL)

	for {
		select {
		case <-ctx.Done():
			return
		case env := <-wsc.Incoming:
			switch env.Type {
			case protocol.TypeBackupNow:
				go runBackup(protocol.SnapshotKindManual, true)
			case protocol.TypeRestore:
				go runRestore(env.SnapshotID)
			case protocol.TypeCancel:
				jobMu.Lock()
				if jobCancel != nil {
					jobCancel()
				}
				jobMu.Unlock()
			case protocol.TypeConfig:
				interval := 0
				if env.IntervalMinutes != nil {
					interval = *env.IntervalMinutes
				}
				retention := 0
				if env.RetentionCount != nil {
					retention = *env.RetentionCount
				}
				pol.set(interval, retention, env.BackupPaths, 0)
			}
		}
	}
}
