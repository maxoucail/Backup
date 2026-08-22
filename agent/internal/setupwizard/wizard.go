// Package setupwizard runs the first-run device enrollment flow: a tiny
// local web page (no console, no native GUI toolkit needed) where the
// operator enters the server address and enrollment key generated from
// the panel. On success the resulting permanent device identity is saved
// to the agent's local config.
package setupwizard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"backup-agent/internal/client"
	"backup-agent/internal/config"
	"backup-agent/internal/osui"
)

//go:embed wizard.html
var wizardHTML []byte

type submitRequest struct {
	ServerURL  string `json:"server_url"`
	Token      string `json:"token"`
	DeviceName string `json:"device_name"`
}

// Run blocks until the operator successfully enrolls the device (or ctx is
// cancelled), and returns the config to persist.
func Run(ctx context.Context, agentVersion string) (*config.Config, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().(*net.TCPAddr)

	resultCh := make(chan *config.Config, 1)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(wizardHTML)
	})

	mux.HandleFunc("POST /submit", func(w http.ResponseWriter, r *http.Request) {
		var req submitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "requête invalide")
			return
		}
		hostname, _ := os.Hostname()
		name := req.DeviceName
		if name == "" {
			name = hostname
		}

		enrollCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		deviceID, deviceSecret, err := client.Enroll(enrollCtx, req.ServerURL, req.Token, name, hostname, runtime.GOOS, "", agentVersion)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "impossible de contacter le serveur ou clé invalide : "+err.Error())
			return
		}

		cfg := &config.Config{
			ServerURL:    req.ServerURL,
			DeviceID:     deviceID,
			DeviceSecret: deviceSecret,
			DeviceName:   name,
		}
		if err := config.Save(cfg); err != nil {
			writeErr(w, http.StatusInternalServerError, "impossible d'enregistrer la configuration : "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		resultCh <- cfg
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	url := "http://" + addr.String() + "/"
	log.Printf("assistant de configuration disponible sur %s", url)
	_ = osui.ShowURL(url)
	osui.Notify("Configuration requise", "Ouvrez "+url+" pour connecter cet appareil à votre serveur de sauvegarde.")

	select {
	case cfg := <-resultCh:
		// Give the browser a moment to render the success page before the
		// local server goes away.
		time.Sleep(1500 * time.Millisecond)
		return cfg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(60 * time.Minute):
		return nil, errors.New("délai de configuration dépassé")
	}
}

func writeErr(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
