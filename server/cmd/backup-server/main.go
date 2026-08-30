// Command backup-server is the NAS-side backup server. It exposes two
// separate HTTP listeners on purpose: the admin web panel (login,
// dashboard, device/settings management) on one port, and everything a
// backed-up workstation's agent needs (enrollment, file upload, the
// WebSocket control channel) on another. An operator can then firewall
// the panel port to an admin-only network/VLAN while leaving the agent
// port reachable from every subnet workstations live on - the two have
// very different "who should be able to reach this" requirements.
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"backup-server/internal/api"
	"backup-server/internal/auth"
	"backup-server/internal/config"
	"backup-server/internal/db"
	"backup-server/internal/filestore"
	"backup-server/internal/models"
	"backup-server/internal/queue"
	"backup-server/internal/scheduler"
	"backup-server/internal/web"
	"backup-server/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	sqlDB, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("base de données: %v", err)
	}
	defer sqlDB.Close()

	if err := db.Bootstrap(sqlDB, cfg.DataDir, cfg.StorageRoot, auth.HashPassword); err != nil {
		log.Fatalf("initialisation: %v", err)
	}

	settings, err := models.GetSettings(sqlDB)
	if err != nil {
		log.Fatalf("lecture des paramètres: %v", err)
	}
	store, err := filestore.New(settings.StorageRoot)
	if err != nil {
		log.Fatalf("stockage (%s): %v", settings.StorageRoot, err)
	}
	storeHolder := filestore.NewHolder(store)

	sessions := auth.NewSessionSigner(cfg.SessionSecret)
	hub := ws.NewHub(sqlDB)

	// Backups are serialized fleet-wide: the queue hands out slots and, as
	// each frees up, tells the next waiting device to start. The limit is
	// read from settings on every decision so changing it in the panel
	// takes effect immediately.
	backupQueue := queue.New(
		func() int {
			s, err := models.GetSettings(sqlDB)
			if err != nil {
				return 1
			}
			return s.MaxConcurrentBackups
		},
		func(deviceID string) bool {
			return hub.SendCommand(deviceID, ws.Envelope{Type: ws.TypeBackupNow})
		},
	)
	// A machine that drops off mid-backup must not hold the queue shut.
	hub.OnDisconnect = backupQueue.Forget
	// A machine reconnecting after having missed its estimated backup slot
	// (asleep, unplugged, off the network) gets told to offer the user a
	// new time right away, instead of silently waiting for its next
	// regularly scheduled attempt.
	hub.OnConnect = func(deviceID string) { offerRescheduleIfOverdue(sqlDB, hub, deviceID) }

	apiServer := api.New(sqlDB, storeHolder, hub, sessions, backupQueue, cfg.DownloadsDir, cfg.AgentPort)
	webServer, err := web.New(sessions)
	if err != nil {
		log.Fatalf("panneau web: %v", err)
	}

	panelMux := http.NewServeMux()
	apiServer.RegisterShared(panelMux)
	apiServer.RegisterPanel(panelMux)
	webServer.RegisterShared(panelMux)
	webServer.RegisterPanel(panelMux)

	agentMux := http.NewServeMux()
	apiServer.RegisterShared(agentMux)
	apiServer.RegisterAgent(agentMux)
	webServer.RegisterShared(agentMux)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go scheduler.Run(ctx, sqlDB, storeHolder, backupQueue)

	panelAddr := cfg.ListenHost + ":" + cfg.PanelPort
	agentAddr := cfg.ListenHost + ":" + cfg.AgentPort

	panelSrv := newServer(panelAddr, panelMux)
	agentSrv := newServer(agentAddr, agentMux)

	go func() {
		log.Printf("panneau web en écoute sur %s (données: %s, stockage: %s)", panelAddr, cfg.DataDir, settings.StorageRoot)
		if err := panelSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serveur panneau: %v", err)
		}
	}()
	go func() {
		log.Printf("trafic agent (enrôlement, sauvegardes, WebSocket) en écoute sur %s", agentAddr)
		if err := agentSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serveur agent: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("arrêt en cours...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := panelSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("arrêt forcé (panneau): %v", err)
	}
	if err := agentSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("arrêt forcé (agent): %v", err)
	}
}

// offerRescheduleIfOverdue checks whether a device that just (re)connected
// has missed its estimated backup slot - the same estimate the dashboard
// shows as "prochaine sauvegarde" (last successful start + effective
// interval) - and if so, tells it to offer the user a new time. A device
// that has never backed up successfully has nothing to be overdue against
// yet, so it's left alone; a fresh connection racing the agent's own
// scheduled attempt is harmless, since offerCatchUpSlot on the agent side
// just opens the same picker it would show on its own.
func offerRescheduleIfOverdue(sqlDB *sql.DB, hub *ws.Hub, deviceID string) {
	device, err := models.GetDevice(sqlDB, deviceID)
	if err != nil {
		return
	}
	settings, err := models.GetSettings(sqlDB)
	if err != nil {
		return
	}
	lastStart, err := models.LastSuccessfulSnapshotStart(sqlDB, deviceID)
	if err != nil || lastStart.IsZero() {
		return
	}
	interval := models.EffectiveIntervalMinutes(device, settings)
	dueAt := lastStart.Add(time.Duration(interval) * time.Minute)
	if time.Now().Before(dueAt) {
		return
	}
	if hub.SendCommand(deviceID, ws.Envelope{Type: ws.TypeOfferReschedule}) {
		_ = models.AddEvent(sqlDB, &deviceID, models.EventLevelInfo,
			"Sauvegarde en retard détectée à la reconnexion : proposition de reprogrammation envoyée.")
	}
}

func newServer(addr string, mux *http.ServeMux) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // multi-gigabyte file uploads can legitimately run long
		IdleTimeout:  120 * time.Second,
	}
}
