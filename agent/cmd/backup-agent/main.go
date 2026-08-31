// Command backup-agent runs on the machine being backed up (Windows or
// macOS, though it builds and runs on Linux too). Installed, it runs as a
// real system service/daemon: starts at boot before anyone logs in, and
// can only be stopped by an administrator. On first run (or after being
// decommissioned from the panel) it walks the operator through a
// local-web-page enrollment wizard; from then on it backs up on a
// schedule, catches up on missed backups with the user's consent, and
// reacts to remote commands (backup now, cancel, decommission) sent from
// the server's panel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"backup-agent/internal/autostart"
	"backup-agent/internal/backupjob"
	"backup-agent/internal/client"
	"backup-agent/internal/config"
	"backup-agent/internal/keepawake"
	"backup-agent/internal/knownfolders"
	"backup-agent/internal/macdaemon"
	"backup-agent/internal/macmenubar"
	"backup-agent/internal/osinfo"
	"backup-agent/internal/osui"
	"backup-agent/internal/progressui"
	"backup-agent/internal/protocol"
	"backup-agent/internal/reschedulewizard"
	"backup-agent/internal/scanner"
	"backup-agent/internal/setupwizard"
	"backup-agent/internal/svcmode"
	"backup-agent/internal/tray"
	"backup-agent/internal/userctx"
	"backup-agent/internal/winsession"
)

// AgentVersion is overridden at build time via -ldflags "-X main.AgentVersion=...".
var AgentVersion = "1.0.0"

const defaultIntervalMinutes = 360
const defaultRetentionCount = 7
const missedBackupGrace = 10 * time.Minute

// queueWaitBeforeOfferingSlot is how long this machine waits for its turn
// in the server's backup queue before treating the wait as "it isn't
// going to happen" and asking the user to pick another time. Long enough
// to sit through another machine's large backup, short enough that the
// user still gets asked within the same working session.
const queueWaitBeforeOfferingSlot = 90 * time.Minute

// trayControlAddr is where the Windows Service exposes its small local
// control API for the tray helper process (last backup date, "back up
// now", "reschedule"). Fixed rather than discovered: the tray helper and
// the service are always the same machine, same install, so there's
// nothing to discover.
const trayControlAddr = "127.0.0.1:47812"
const trayControlBaseURL = "http://" + trayControlAddr

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--show-url": // internal: invoked by the service in the console session
			if len(os.Args) > 2 {
				_ = osui.OpenBrowser(os.Args[2])
			}
			return
		case "--tray": // internal: the notification-area icon helper, launched by the service
			_ = tray.Run(trayControlBaseURL)
			return
		case "--menubar": // internal: the macOS menu bar icon helper, launched by the LaunchDaemon
			if err := macmenubar.Run(trayControlBaseURL); err != nil {
				log.Printf("barre de menu: %v", err)
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

func setupLogging(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if dir, err := config.ServiceLogDir(); err == nil {
		setupLogging(dir)
	}
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("chemin de l'exécutable introuvable: %v", err)
	}

	switch runtime.GOOS {
	case "windows":
		userctx.HomeDir = winsession.ConsoleUserHomeDir
		// Re-resolved on every lookup, not captured once here: the service
		// starts at boot with nobody logged in, so a one-shot value stays
		// empty for the whole service lifetime and every registry read
		// silently falls back to the SYSTEM hive.
		knownfolders.UserSIDFunc = winsession.ConsoleUserSID
		if sid, err := winsession.ConsoleUserSID(); err == nil {
			knownfolders.UserSID = sid
		} else {
			log.Printf("aucune session utilisateur au démarrage du service (%v) : le SID sera résolu à la première session ouverte", err)
		}
		osui.ShowURL = func(url string) error {
			return winsession.LaunchInConsoleSession(exePath, []string{"--show-url", url})
		}
		go ensureTrayHelperRunning(ctx, exePath)
	case "darwin":
		userctx.HomeDir = macdaemon.ConsoleUserHomeDir
		osui.ShowURL = func(url string) error {
			return macdaemon.LaunchInConsoleSession(exePath, []string{"--show-url", url})
		}
		go ensureMenuBarHelperRunning(ctx, exePath)
	}

	log.Printf("backup-agent %s démarré en service système", AgentVersion)
	mainLoop(ctx)
}

// ensureTrayHelperRunning launches the notification-area icon into the
// console user's session. Retries with backoff until it succeeds (there
// may be no one logged in yet right after boot); once launched it isn't
// re-supervised for the rest of this service run - if the user logs off
// and back on, the icon reappears at the next service restart. A fuller
// version would watch WTS session-change notifications; this simpler
// version was judged good enough for a first pass.
func ensureTrayHelperRunning(ctx context.Context, exePath string) {
	// A previous run's helper (this service restarting, a crash, an
	// update) is not a child of this process and so was never reaped -
	// without this, every restart piled on another icon instead of
	// replacing the last one.
	tray.KillRunningHelper()

	delay := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := winsession.LaunchInConsoleSession(exePath, []string{"--tray"}); err != nil {
			delay = 30 * time.Second
			continue
		}
		log.Print("icône de la barre des tâches lancée dans la session utilisateur")
		return
	}
}

// ensureMenuBarHelperRunning is ensureTrayHelperRunning's macOS
// counterpart: launches the menu bar icon into the console user's
// session, retrying with backoff since nobody may be logged in yet right
// after boot.
func ensureMenuBarHelperRunning(ctx context.Context, exePath string) {
	delay := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := macdaemon.LaunchInConsoleSession(exePath, []string{"--menubar"}); err != nil {
			delay = 30 * time.Second
			continue
		}
		log.Print("icône de la barre de menu lancée dans la session utilisateur")
		return
	}
}

// runForeground is the per-user path: a manual run, a dev/test run, or
// the Linux fallback. It keeps the original per-user autostart behaviour
// so a plain double-click / systemd --user unit still works standalone.
func runForeground(ctx context.Context) {
	if dir, err := config.Dir(); err == nil {
		setupLogging(dir)
	}
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
}

func newPolicy() *policy {
	return &policy{intervalMinutes: defaultIntervalMinutes, retentionCount: defaultRetentionCount}
}

func (p *policy) set(interval, retention int, paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if interval > 0 {
		p.intervalMinutes = interval
	}
	if retention > 0 {
		p.retentionCount = retention
	}
	p.backupPaths = paths
}

func (p *policy) snapshot() (interval int, paths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.intervalMinutes, p.backupPaths
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

	// queuedSince tracks a scheduled backup that the server put in the
	// queue. If our turn never comes (the machine is about to be shut
	// down, the queue stays busy all evening), we offer the user another
	// slot rather than silently skipping the backup entirely.
	var queuedMu sync.Mutex
	var queuedSince time.Time
	markQueued := func(queued bool) {
		queuedMu.Lock()
		defer queuedMu.Unlock()
		if queued {
			if queuedSince.IsZero() {
				queuedSince = time.Now()
			}
			return
		}
		queuedSince = time.Time{}
	}
	queuedFor := func() time.Duration {
		queuedMu.Lock()
		defer queuedMu.Unlock()
		if queuedSince.IsZero() {
			return 0
		}
		return time.Since(queuedSince)
	}

	// offerCatchUpSlot asks the user to pick a time to make up a backup
	// that didn't happen - whether it was missed while the machine was
	// off, failed, or never got its turn in the server's queue. All three
	// end the same way: the machine has no fresh backup and someone should
	// decide when it gets one. Guarded so overlapping triggers don't stack
	// several wizards on screen.
	var catchUpWizardOpen atomic.Bool
	offerCatchUpSlot := func(reason string) {
		if !catchUpWizardOpen.CompareAndSwap(false, true) {
			return
		}
		defer catchUpWizardOpen.Store(false)

		log.Printf("proposition d'un nouveau créneau : %s", reason)
		picked, err := reschedulewizard.Run(agentCtx)
		if err != nil || picked == nil || agentCtx.Err() != nil {
			return
		}
		chosen := *picked
		mutateCfg(func(c *config.Config) {
			c.PendingCatchUpAt = &chosen
			c.NextScheduledAt = &chosen
			c.CatchUpNotifiedT15, c.CatchUpNotifiedT5 = false, false
		})
		log.Printf("sauvegarde reprogrammée pour %s", chosen.Format(time.RFC1123))
	}

	// Last-backup state, surfaced to the tray icon's tooltip/menu via the
	// control API below.
	var lastBackupMu sync.Mutex
	var lastBackupAt time.Time
	var lastBackupStatus string
	recordLastBackup := func(status string) {
		lastBackupMu.Lock()
		lastBackupAt, lastBackupStatus = time.Now(), status
		lastBackupMu.Unlock()
	}
	readLastBackup := func() (time.Time, string) {
		lastBackupMu.Lock()
		defer lastBackupMu.Unlock()
		return lastBackupAt, lastBackupStatus
	}

	warnIfBlocked := func(roots []scanner.Root) {
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

		// A machine that idle-sleeps mid-upload doesn't pause the backup and
		// resume it later - the connection just drops, and the run can end
		// up looking finished (with a pile of skipped files) rather than
		// interrupted. Held for the whole backup, released the moment it
		// ends either way.
		stopKeepAwake := keepawake.Start()
		defer stopKeepAwake()

		popup := existingPopup
		if popup == nil && kind == protocol.SnapshotKindManual {
			popup, _ = progressui.Show("Sauvegarde en cours")
		}
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupStarted})

		_, configuredPaths := pol.snapshot()
		roots := scanner.ResolveRoots(configuredPaths)
		warnIfBlocked(roots)

		result, err := backupjob.Run(jobCtx, api, kind, roots, func(p backupjob.Progress) {
			if popup != nil {
				popup.Update(p.Phase, p.Percent, p.EtaSeconds, p.UploadedBytes, p.BytesPerSec)
			}
			wsc.Send(protocol.Envelope{
				Type: protocol.TypeProgress, SnapshotID: p.SnapshotID, Phase: p.Phase, FileCount: p.FileCount,
				LogicalBytes: p.LogicalBytes, UploadedBytes: p.UploadedBytes, Percent: p.Percent, EtaSeconds: p.EtaSeconds,
			})
		})

		// Queued behind another machine: the server holds our place and
		// will start us when a slot frees. Nothing failed, so don't report
		// a failed backup or close the popup on an error.
		if errors.Is(err, client.ErrQueued) {
			log.Printf("sauvegarde en attente: %v", err)
			if popup != nil {
				popup.Finish("") // the popup's job is done; the real run gets its own
			}
			markQueued(true)
			return
		}
		markQueued(false)

		status, errMsg := protocol.SnapshotStatusSuccess, ""
		if err != nil {
			status, errMsg = protocol.SnapshotStatusFailed, err.Error()
			checkAuth(err)
			log.Printf("sauvegarde échouée: %v", err)
			// warnIfBlocked above only catches a folder that can't be
			// listed at all - macOS lets that succeed for the protected
			// folders even without Full Disk Access, so a revoked grant
			// only shows up here, once most individual file reads have
			// actually failed. Without this, that run would otherwise
			// just look like a quiet, mostly-empty "success" with no clear
			// signal anything was wrong.
			if errors.Is(err, backupjob.ErrPermissionDenied) {
				osui.Notify("Sauvegarde bloquée (accès refusé)",
					"La plupart des fichiers n'ont pas pu être lus. Ouvrez Réglages Système → Confidentialité et sécurité → Accès complet au disque, et autorisez Backup Agent, puis relancez une sauvegarde.")
			}
		} else {
			log.Printf("sauvegarde terminée: %d fichiers, %d octets envoyés", result.FileCount, result.UploadedBytes)
			if n := len(result.SkippedFiles); n > 0 {
				// Surfaced as a warning rather than swallowed: the snapshot
				// is usable, but the operator should know these files
				// aren't in it (typically open/locked during the run).
				sample := result.SkippedFiles
				if len(sample) > 5 {
					sample = sample[:5]
				}
				wsc.Send(protocol.Envelope{
					Type: protocol.TypeLog, Level: protocol.LevelWarning,
					Message: fmt.Sprintf("%d fichier(s) ignoré(s) (modifiés ou verrouillés pendant la sauvegarde), par exemple : %s",
						n, strings.Join(sample, ", ")),
				})
			}
		}
		recordLastBackup(status)
		wsc.Send(protocol.Envelope{Type: protocol.TypeBackupFinished, Status: status, ErrorMessage: errMsg})
		if popup != nil {
			popup.Finish(errMsg)
		}

		if kind == protocol.SnapshotKindScheduled {
			interval, _ := pol.snapshot()
			next := time.Now().Add(time.Duration(interval) * time.Minute)
			mutateCfg(func(c *config.Config) {
				c.NextScheduledAt = &next
				c.PendingCatchUpAt = nil
				c.CatchUpNotifiedT15, c.CatchUpNotifiedT5 = false, false
			})

			// A scheduled backup that failed would otherwise just wait for
			// the next interval, leaving the machine unprotected until then
			// without anyone being asked. Offer a specific catch-up slot,
			// the same way a backup missed while powered off is handled.
			// Runs detached: the wizard waits on a human, and this
			// goroutine still holds the agent's single-job slot.
			if err != nil {
				go offerCatchUpSlot("Votre dernière sauvegarde n'a pas pu être terminée.")
			}
		}
	}

	if (runtime.GOOS == "windows" && svcmode.IsWindowsService()) || (runtime.GOOS == "darwin" && macdaemon.IsRoot()) {
		startTrayControlAPI(agentCtx, cfg, mutateCfg, wsc, runBackup, readLastBackup)
	}

	refreshPolicy := func() {
		reqCtx, cancel := context.WithTimeout(agentCtx, 20*time.Second)
		defer cancel()
		resp, err := api.GetConfig(reqCtx)
		if err != nil {
			checkAuth(err)
			return
		}
		pol.set(resp.IntervalMinutes, resp.RetentionCount, resp.BackupPaths)
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

	// Waiting in the server's queue is normal and usually short. But if our
	// turn genuinely never comes - the queue stays saturated all evening,
	// or the machine is heading for shutdown - the backup would silently
	// not happen. Past a threshold, ask the user for a slot instead.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-agentCtx.Done():
				return
			case <-ticker.C:
				if queuedFor() > queueWaitBeforeOfferingSlot {
					markQueued(false)
					offerCatchUpSlot("Le tour de cette machine dans la file d'attente n'est pas venu.")
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
			interval, _ := pol.snapshot()
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
				pol.set(interval, retention, env.BackupPaths)
			case protocol.TypeOfferReschedule:
				go offerCatchUpSlot("le serveur signale que la machine était hors ligne au moment prévu de la sauvegarde")
			}
		}
	}
}

// startTrayControlAPI exposes the small local HTTP API the tray helper
// process (internal/tray, launched into the console session) talks to:
// it has no other way to reach the service, and this is deliberately
// tiny (five endpoints) since anything reachable on localhost by *any*
// process on the machine should stay minimal. A backup triggered here
// goes through the exact same runBackup as a remote "Sauvegarder
// maintenant" from the panel - the server's own commands (backup_now,
// cancel) are handled independently in the main WS loop and are
// never blocked or delayed by anything the tray does, which is what
// keeps the server always authoritative.
func startTrayControlAPI(
	ctx context.Context, cfg *config.Config, mutateCfg func(func(*config.Config)),
	wsc *client.WSClient, runBackup func(kind string, popup *progressui.Popup),
	readLastBackup func() (time.Time, string),
) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /tray/state", func(w http.ResponseWriter, r *http.Request) {
		at, status := readLastBackup()
		lastAt := ""
		if !at.IsZero() {
			lastAt = at.Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_name":        cfg.DeviceName,
			"last_backup_at":     lastAt,
			"last_backup_status": status,
			"connected":          wsc.Connected(),
		})
	})

	mux.HandleFunc("POST /tray/backup-now", func(w http.ResponseWriter, r *http.Request) {
		go runBackup(protocol.SnapshotKindManual, nil)
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /tray/reschedule-page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(trayReschedulePageHTML))
	})

	mux.HandleFunc("POST /tray/reschedule", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			At string `json:"at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		t, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mutateCfg(func(c *config.Config) {
			c.PendingCatchUpAt = &t
			c.NextScheduledAt = &t
			c.CatchUpNotifiedT15, c.CatchUpNotifiedT5 = false, false
		})
		log.Printf("prochaine sauvegarde reprogrammée depuis la barre des tâches pour %s", t.Format(time.RFC1123))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// Redirects to the server's self-service download page, not to
	// cfg.ServerURL bare - that's the agent-facing API root, which has no
	// page of its own to show a browser (a bare GET there 404s). /download
	// is the one page on that same host and port meant to be opened by a
	// human: no login required, reachable by any workstation being backed
	// up, exactly like the page this agent itself was installed from.
	mux.HandleFunc("GET /tray/open-panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.TrimRight(cfg.ServerURL, "/")+"/download", http.StatusFound)
	})

	srv := &http.Server{Addr: trayControlAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("API de contrôle pour la barre des tâches indisponible: %v", err)
		}
	}()
}

const trayReschedulePageHTML = `<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Reprogrammer — Backup Agent</title>
<style>
	:root { --bg:#0b0f14; --panel:#121821; --border:#232d3b; --text:#e6edf3; --dim:#8b98a9; --accent:#4f8cff; --green:#33c481; }
	* { box-sizing:border-box; } body { margin:0; background:var(--bg); color:var(--text); font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif; min-height:100vh; display:flex; align-items:center; justify-content:center; }
	.card { width:400px; max-width:calc(100vw - 40px); background:var(--panel); border:1px solid var(--border); border-radius:14px; padding:28px; }
	h1 { font-size:1.1rem; margin:0 0 14px 0; }
	label { display:block; font-size:.82rem; color:var(--dim); margin:0 0 6px 0; }
	input { width:100%; background:var(--bg); border:1px solid var(--border); color:var(--text); padding:9px 12px; border-radius:8px; font-size:.9rem; }
	button { width:100%; margin-top:16px; background:var(--accent); color:#fff; border:none; padding:11px; border-radius:8px; font-weight:600; cursor:pointer; }
	.msg { margin-top:10px; font-size:.85rem; text-align:center; }
	.footer { text-align:center; margin-top:20px; font-size:.72rem; color:var(--dim); opacity:.7; }
</style></head>
<body><div class="card">
	<h1>Reprogrammer la prochaine sauvegarde</h1>
	<label>Date et heure</label>
	<input type="datetime-local" id="dt">
	<button id="go">Programmer</button>
	<div class="msg" id="msg"></div>
	<div class="footer">&copy; Dallaverde &mdash; Backup Agent</div>
</div>
<script>
function pad(n){return String(n).padStart(2,"0");}
const now = new Date();
document.getElementById("dt").min = now.getFullYear()+"-"+pad(now.getMonth()+1)+"-"+pad(now.getDate())+"T"+pad(now.getHours())+":"+pad(now.getMinutes());
document.getElementById("dt").value = document.getElementById("dt").min;
document.getElementById("go").addEventListener("click", async () => {
	const v = document.getElementById("dt").value;
	const msg = document.getElementById("msg");
	if (!v) { msg.textContent = "Choisissez une date."; return; }
	try {
		const res = await fetch("/tray/reschedule", { method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify({at: new Date(v).toISOString()}) });
		if (!res.ok) throw new Error("échec");
		msg.textContent = "Programmé. Vous pouvez fermer cette page.";
	} catch (e) { msg.textContent = "Erreur, réessayez."; }
});
</script></body></html>`
