// Package reschedulewizard shows the "your last backup couldn't run"
// local web page after a missed scheduled backup is detected (machine was
// off or asleep through the whole window), letting the operator pick a
// specific catch-up time - today or another day - or dismiss it and wait
// for the normal schedule to resume.
package reschedulewizard

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"backup-agent/internal/osui"
)

//go:embed reschedule.html
var pageHTML []byte

type submitRequest struct {
	ScheduledAt *string `json:"scheduled_at"`
	Skip        bool    `json:"skip"`
}

// Result is nil (with ok=false) if the operator chose to skip rescheduling.
func Run(ctx context.Context) (chosen *time.Time, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	resultCh := make(chan *time.Time, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(pageHTML)
	})
	mux.HandleFunc("POST /submit", func(w http.ResponseWriter, r *http.Request) {
		var req submitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "requête invalide"})
			return
		}
		var picked *time.Time
		if !req.Skip && req.ScheduledAt != nil {
			t, err := time.Parse(time.RFC3339, *req.ScheduledAt)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "date invalide"})
				return
			}
			picked = &t
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		resultCh <- picked
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	url := "http://" + ln.Addr().String() + "/"
	log.Printf("assistant de reprogrammation disponible sur %s", url)
	_ = osui.ShowURL(url)
	osui.Notify("Sauvegarde manquée", "Votre dernière sauvegarde n'a pas pu être effectuée. Choisissez un nouveau créneau : "+url)

	select {
	case picked := <-resultCh:
		time.Sleep(1500 * time.Millisecond)
		return picked, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Hour):
		return nil, nil // gave up waiting for a response; just skip silently
	}
}
