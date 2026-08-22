// Command backup-agent runs on the machine being backed up (Windows or
// macOS, though it builds and runs on Linux too). Installed, it runs as a
// real system service/daemon: starts at boot before anyone logs in, and
// can only be stopped by an administrator. On first run (or after being
// decommissioned from the panel) it walks the operator through a
// local-web-page enrollment wizard; from then on it backs up on a
// schedule, catches up on missed backups with the user's consent, and
// reacts to remote commands (backup now, restore, decommission) sent from
// the server's panel.
package main

import (
	"context"
	"errors"
	"fmt"
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
	"backup-agent/internal/macdaemon"
	"backup-agent/internal/osinfo"
	"backup-agent/internal/osui"
	"backup-agent/internal/progressui"
	"backup-agent/internal/protocol"
	"backup-agent/internal/reschedulewizard"
	"backup-agent/internal/restorejob"
	"backup-agent/internal/scanner"
	"backup-agent/internal/setupwizard"
	"backup-agent/internal/svcmode"
	"backup-agent/internal/userctx"
	"backup-agent/internal/winsession"
)

// AgentVersion is overridden at build time via -ldflags "-X main.AgentVersion=...".
var AgentVersion = "1.0.0"

const defaultIntervalMinutes = 360
const defaultRetentionCount = 7
const defaultChunkSize = 16 * 1024 * 1024
const missedBackupGrace = 10 * time.Minute

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--show-url": // internal: invoked by the service in the console session
			if len(os.Args) > 2 {
				_ = osui.OpenBrowser(os.Args[2])
			}
			return
		case "install":
			if err := installSelf(); err != nil {
				fmt.Fprintln(os.Stderr, "installation échouée:", err)
				os.Exit(1)
			}
			fmt.Println("Backup Agent installé et démarré en service.")
			return
		case "uninstall":
			if err := uninstallSelf(); err != nil {
				fmt.Fprintln(os.Stderr, "désinstallation échouée:", err)
				os.Exit(1)
			}
			fmt.Println("Backup Agent désinstallé.")
			return
		}
	}

	if runtime.GOOS == "windows" && svcmode.IsWindowsService() {
		if err := svcmode.Run(runServiceMode); err != nil {
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if runtime.GOOS == "darwin" && macdaemon.IsRoot() {
		runServiceMode(ctx)
		return
	}

	runForeground(ctx)
}

func installSelf() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		return svcmode.Install(exePath)
	case "darwin":
		return macdaemon.Install(exePath)
	default:
		return autostart.Ensure(exePath) // Linux: no privileged service model targeted, per-user autostart only
	}
}

func uninstallSelf() error {
	switch runtime.GOOS {
	case "windows":
		return svcmode.Uninstall()
	case "darwin":
		return macdaemon.Uninstall()
	default:
		return autostart.Remove()
	}
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

// runServiceMode is entered when the binary is running privileged and
// unattended (Windows Service under LocalSystem, or macOS LaunchDaemon
// under root): there is no interactive session of its own, so it must
// resolve the console user's home directory instead of its own, and cross
// into that user's session to show anything on screen.
func runServiceMode(ctx context.Context) {
	setupLogging()
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("chemin de l'exécutable introuvable: %v", err)
	}

	switch runtime.GOOS {
	case "windows":
		userctx.HomeDir = winsession.ConsoleUserHomeDir
		osui.ShowURL = func(url string) error {
			return winsession.LaunchInConsoleSession(exePath, []string{"--show-url", url})
		}
	case "darwin":
		userctx.HomeDir = macdaemon.ConsoleUserHomeDir
		osui.ShowURL = func(url string) error {
			return macdaemon.LaunchInConsoleSession(exePath, []string{"--show-url", url})
		}
	}

	log.Printf("backup-agent %s démarré en service système", AgentVersion)
	mainLoop(ctx)
}

// runForeground is the per-user path: a manual run, a dev/test run, or
// the Linux fallback. It keeps the original per-user autostart behaviour
// so a plain double-click / systemd --user unit still works standalone.
func runForeground(ctx context.Context) {
	setupLogging()
	if exePath, err := os.Executable(); err == nil {
		if err := autostart.Ensure(exePath); err != nil {
			log.Printf("avertissement: démarrage automatique non configuré: %v", err)
		}
	}
	mainLoop(ctx)
}

func mainLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		cfg, err := config.Load()
		if err != nil {
			log.Fatalf("configuration: %v", err)
		}

		if !cfg.Enrolled() {
			log.Print("appareil non enrôlé, ouverture de l'assistant de configuration...")
			newCfg, err := setupwizard.Run(ctx, AgentVersion)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("assistant de configuration: %v (nouvelle tentative dans 1 minute)", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
				continue
			}
			cfg = newCfg
			log.Print("enrôlement réussi.")
		}

		reason := runAgent(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if reason == exitReenroll {
			log.Print("cet appareil n'est plus reconnu par le serveur : réinitialisation et nouvel enrôlement.")
			_ = config.Clear()
			continue
		}
		return
	}
}

type exitReason int

const (
	exitStopped exitReason = iota
	exitReenroll
)

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

func runAgent(ctx context.Context, cfg *config.Config) exitReason {
	agentCtx, cancelAgent := context.WithCancel(ctx)
	defer cancelAgent()

	// cfg's scheduling fields (NextScheduledAt, PendingCatchUpAt, the
	// catch-up notification flags) are read and written from several
	// goroutines below (the regular ticker, the catch-up watcher, and
	// whichever goroutine a WS command or the ticker fires runBackup
	// from) - cfgMu is the single lock guarding all of that state.
	var cfgMu sync.Mutex
	mutateCfg := func(fn func(*config.Config)) {
		cfgMu.Lock()
		fn(cfg)
		_ = config.Save(cfg)
		cfgMu.Unlock()
	}
	nextScheduledAt := func() *time.Time {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.NextScheduledAt
	}

	reenroll := make(chan struct{}, 1)
	flagUnauthorized := func() {
		select {
		case reenroll <- struct{}{}:
		default:
		}
		cancelAgent()
	}

	hostname, _ := os.Hostname()
	hello := protocol.Envelope{
		Type: protocol.TypeHello, Hostname: hostname,
		OSName: runtime.GOOS, OSVersion: osinfo.Version(), AgentVersion: AgentVersion,
	}

	wsc := client.NewWSClient(cfg.ServerURL, cfg.DeviceID, cfg.DeviceSecret, hello)
	go wsc.Run(agentCtx)
	go func() {
		select {
		case <-agentCtx.Done():
		case <-wsc.Unauthorized:
			flagUnauthorized()
		}
	}()

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
		jobCtx, cancel := context.WithCancel(agentCtx)
		jobCancel = cancel
		done := func() {
			jobMu.Lock()
			jobRunning = false
			jobCancel = nil
			jobMu.Unlock()
		}
		return jobCtx, done, true
	}

	checkAuth := func(err error) {
		if errors.Is(err, client.ErrUnauthorized) {
			flagUnauthorized()
		}
	}

	warnIfBlocked := func(roots []string) {
		blocked := scanner.CheckAccess(roots)
		if len(blocked) == 0 {
			return
		}
		msg := "Certains dossiers ne peuvent pas être lus (accès refusé) : " + fmt.Sprint(blocked) +
			". Sur macOS, autorisez l'accès complet au disque pour Backup Agent dans Réglages Système → Confidentialité et sécurité."
		log.Print(msg)
		wsc.Send(protocol.Envelope{Type: protocol.TypeLog, Level: protocol.LevelWarning, Message: msg})
		osui.Notify("Accès aux fichiers requis", "Ouvrez Réglages Système → Confidentialité et sécurité → Accès complet au disque, et autorisez Backup Agent.")
	}

	// runBackup optionally reuses an already-visible popup (the countdown
	// popup for a scheduled catch-up transitions straight into progress
	// instead of flashing a second window).
	runBackup := func(kind string, existingPopup *progressui.Popup) {
		jobCtx, done, ok := tryStartJob()
		if !ok {
			wsc.Send(protocol.Envelope{Type: protocol.TypeLog, Level: protocol.LevelWarning, Message: "Sauvegarde ignorée : une tâche est déjà en cours."})
			return
		}
		defer done()

		popup := existingPopup
		if popup == nil && kind == protocol.SnapshotKindManual {
			popup, _ = progressui.Show("Sauvegarde en cours")
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupStarted})

		_, configuredPaths, chunkSize := pol.snapshot()
		roots := scanner.ResolveRoots(configuredPaths)
		warnIfBlocked(roots)

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
			checkAuth(err)
			log.Printf("sauvegarde échouée: %v", err)
		} else {
			log.Printf("sauvegarde terminée: %d fichiers, %d octets envoyés", result.FileCount, result.UploadedBytes)
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupFinished, Status: status, ErrorMessage: errMsg})
		if popup != nil {
			popup.Finish(errMsg)
		}

		if kind == protocol.SnapshotKindScheduled {
			interval, _, _ := pol.snapshot()
			next := time.Now().Add(time.Duration(interval) * time.Minute)
			mutateCfg(func(c *config.Config) {
				c.NextScheduledAt = &next
				c.PendingCatchUpAt = nil
				c.CatchUpNotifiedT15, c.CatchUpNotifiedT5 = false, false
			})
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
			checkAuth(err)
			log.Printf("restauration échouée: %v", err)
		} else {
			log.Printf("restauration terminée: %d fichiers restaurés", result.FileCount)
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeRestoreFinished, SnapshotID: snapshotID, Status: status, ErrorMessage: errMsg})
		popup.Finish(errMsg)
	}

	refreshPolicy := func() {
		reqCtx, cancel := context.WithTimeout(agentCtx, 20*time.Second)
		defer cancel()
		resp, err := api.GetConfig(reqCtx)
		if err != nil {
			checkAuth(err)
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
			case <-agentCtx.Done():
				return
			case <-ticker.C:
				refreshPolicy()
			}
		}
	}()

	pendingCatchUpAt := func() *time.Time {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.PendingCatchUpAt
	}
	catchUpFlags := func() (t15, t5 bool) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return cfg.CatchUpNotifiedT15, cfg.CatchUpNotifiedT5
	}

	// missedBackupResolved gates the regular scheduler below: it must not
	// decide "next is overdue, run now" behind the operator's back while
	// the missed-backup wizard is still waiting on their answer. It's
	// closed once the initial check below is done, one way or another
	// (nothing missed / rescheduled / skipped) - not once a rescheduled
	// catch-up itself has run, which is tracked separately by
	// PendingCatchUpAt and can take hours.
	missedBackupResolved := make(chan struct{})

	// Missed-backup detection: if the last scheduled backup's due time has
	// already passed by more than a grace window, the machine was almost
	// certainly off or asleep through it. Ask the operator when to make it
	// up instead of silently skipping straight to the next cycle.
	go func() {
		now := time.Now()
		if pendingCatchUpAt() == nil {
			if next := nextScheduledAt(); next != nil && now.Sub(*next) > missedBackupGrace {
				picked, err := reschedulewizard.Run(agentCtx)
				if err != nil || agentCtx.Err() != nil {
					close(missedBackupResolved)
					return
				}
				if picked != nil {
					chosen := *picked
					mutateCfg(func(c *config.Config) {
						c.PendingCatchUpAt = &chosen
						// Also push the regular due-date out to the same
						// time, so the plain scheduler below doesn't see
						// the still-overdue old date and fire a second,
						// unwanted immediate run racing this catch-up.
						c.NextScheduledAt = &chosen
						c.CatchUpNotifiedT15, c.CatchUpNotifiedT5 = false, false
					})
					log.Printf("sauvegarde manquée reprogrammée pour %s", chosen.Format(time.RFC1123))
				} else {
					soon := now.Add(time.Minute)
					mutateCfg(func(c *config.Config) { c.NextScheduledAt = &soon })
				}
			}
		}
		close(missedBackupResolved)

		// T-15/T-5 heads-up and the catch-up run itself.
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		var popup *progressui.Popup
		for {
			select {
			case <-agentCtx.Done():
				return
			case <-ticker.C:
				target := pendingCatchUpAt()
				if target == nil {
					continue
				}
				remaining := time.Until(*target)

				if remaining <= 0 {
					if popup == nil {
						popup, _ = progressui.Show("Sauvegarde programmée")
					}
					mutateCfg(func(c *config.Config) { c.PendingCatchUpAt = nil })
					runBackup(protocol.SnapshotKindScheduled, popup)
					popup = nil
					continue
				}
				t15, t5 := catchUpFlags()
				if remaining <= 5*time.Minute && !t5 {
					mutateCfg(func(c *config.Config) { c.CatchUpNotifiedT5 = true })
					popup, _ = progressui.Show("Sauvegarde programmée")
					popup.SetWaiting(*target)
				}
				if remaining <= 15*time.Minute && !t15 {
					mutateCfg(func(c *config.Config) { c.CatchUpNotifiedT15 = true })
					osui.Notify("Sauvegarde programmée", "Veuillez laisser votre ordinateur allumé : une sauvegarde de rattrapage démarrera à "+target.Format("15:04")+".")
				}
			}
		}
	}()

	// Regular schedule.
	go func() {
		select {
		case <-agentCtx.Done():
			return
		case <-missedBackupResolved:
		}
		if nextScheduledAt() == nil {
			go runBackup(protocol.SnapshotKindScheduled, nil)
		}
		for {
			interval, _, _ := pol.snapshot()
			wait := time.Duration(interval) * time.Minute
			if next := nextScheduledAt(); next != nil {
				if d := time.Until(*next); d > 0 {
					wait = d
				} else {
					wait = 0
				}
			}
			select {
			case <-agentCtx.Done():
				return
			case <-time.After(wait):
				// A pending catch-up (with its own countdown popup) owns
				// firing at this due time; avoid a redundant concurrent
				// attempt racing it and just re-evaluate shortly after.
				if pendingCatchUpAt() != nil {
					time.Sleep(2 * time.Second)
					continue
				}
				runBackup(protocol.SnapshotKindScheduled, nil)
			}
		}
	}()

	log.Printf("backup-agent %s démarré (appareil %s, serveur %s)", AgentVersion, cfg.DeviceID, cfg.ServerURL)

	for {
		select {
		case <-agentCtx.Done():
			select {
			case <-reenroll:
				return exitReenroll
			default:
				return exitStopped
			}
		case env := <-wsc.Incoming:
			switch env.Type {
			case protocol.TypeBackupNow:
				go runBackup(protocol.SnapshotKindManual, nil)
			case protocol.TypeRestore:
				go runRestore(env.SnapshotID)
			case protocol.TypeCancel:
				jobMu.Lock()
				if jobCancel != nil {
					jobCancel()
				}
				jobMu.Unlock()
			case protocol.TypeUninstall:
				log.Print("décommissionnement demandé par le serveur : désinstallation...")
				wsc.Send(protocol.Envelope{Type: protocol.TypeLog, Level: protocol.LevelInfo, Message: "Désinstallation en cours suite à une demande du panneau."})
				time.Sleep(500 * time.Millisecond) // best-effort: let the message reach the server
				if err := uninstallSelf(); err != nil {
					log.Printf("désinstallation du service: %v", err)
				}
				_ = config.Clear()
				os.Exit(0)
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
