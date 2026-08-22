// Command backup-server is the NAS-side backup server: it exposes the
// admin web panel, the agent enrollment/backup/restore HTTP API, and the
// agent WebSocket control channel, all from a single static binary.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"backup-server/internal/api"
	"backup-server/internal/auth"
	"backup-server/internal/config"
	"backup-server/internal/db"
	"backup-server/internal/models"
	"backup-server/internal/scheduler"
	"backup-server/internal/storage"
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
	store, err := storage.New(settings.StorageRoot)
	if err != nil {
		log.Fatalf("stockage (%s): %v", settings.StorageRoot, err)
	}
	storeHolder := storage.NewHolder(store)

	sessions := auth.NewSessionSigner(cfg.SessionSecret)
	hub := ws.NewHub(sqlDB)
	apiServer := api.New(sqlDB, storeHolder, hub, sessions)
	webServer, err := web.New(sessions)
	if err != nil {
		log.Fatalf("panneau web: %v", err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	webServer.Register(mux)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go scheduler.Run(ctx, sqlDB, storeHolder)

	addr := cfg.ListenHost + ":" + cfg.ListenPort
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // chunk uploads/downloads and long polling can legitimately run long
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("backup-server en écoute sur %s (données: %s, stockage: %s)", addr, cfg.DataDir, settings.StorageRoot)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serveur HTTP: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("arrêt en cours...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("arrêt forcé: %v", err)
	}
}
