// Package progressui shows a live progress popup on the user's screen for
// a manually or remotely triggered backup: a tiny local web page,
// auto-opened in the default browser, polling an in-memory state this
// package exposes. Routine scheduled backups run silently and never open
// this - it's reserved for the case the user (or the operator, from the
// panel) explicitly asked to watch happen right now.
package progressui

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"backup-agent/internal/osui"
)

//go:embed popup.html
var popupHTML []byte

type State struct {
	Title           string  `json:"title"`
	Phase           string  `json:"phase"`
	Percent         float64 `json:"percent"`
	EtaSeconds      int64   `json:"eta_seconds"`
	BytesPerSec     float64 `json:"bytes_per_sec"`
	DetailBytes     int64   `json:"detail_bytes"`
	ScheduledAtUnix int64   `json:"scheduled_at_unix,omitempty"`
	Done            bool    `json:"done"`
	Error           string  `json:"error,omitempty"`
}

type Popup struct {
	mu    sync.Mutex
	state State
	srv   *http.Server
	ln    net.Listener
}

// Show starts the local popup server, opens it in the browser, and fires
// a native OS notification as a fallback heads-up in case the browser tab
// doesn't grab focus.
func Show(title string) (*Popup, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Popup{state: State{Title: title}, ln: ln}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(popupHTML)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		s := p.state
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(ln) }()

	url := "http://" + ln.Addr().String() + "/"
	log.Printf("popup de progression disponible sur %s", url)
	_ = osui.ShowURL(url)
	osui.Notify(title, "Suivez la progression : "+url)

	return p, nil
}

func (p *Popup) Update(phase string, percent float64, etaSeconds, detailBytes int64, bytesPerSec float64) {
	p.mu.Lock()
	p.state.Phase = phase
	p.state.Percent = percent
	p.state.EtaSeconds = etaSeconds
	p.state.BytesPerSec = bytesPerSec
	p.state.DetailBytes = detailBytes
	p.state.ScheduledAtUnix = 0
	p.mu.Unlock()
}

// SetWaiting puts the popup into a live countdown display until target,
// used for the 5-minute-before heads-up on a rescheduled catch-up backup.
// The countdown itself ticks client-side in the browser; the agent just
// needs to call Update with the first real progress phase once the job
// actually starts to replace this view.
func (p *Popup) SetWaiting(target time.Time) {
	p.mu.Lock()
	p.state.Phase = "waiting"
	p.state.ScheduledAtUnix = target.Unix()
	p.mu.Unlock()
}

// Finish marks the job done (success if errMsg is empty) and closes the
// local server a little later, giving the open tab time to render the
// final state.
func (p *Popup) Finish(errMsg string) {
	p.mu.Lock()
	p.state.Done = true
	p.state.Error = errMsg
	p.state.Percent = 100
	p.mu.Unlock()

	go func() {
		time.Sleep(30 * time.Second)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.srv.Shutdown(shutdownCtx)
	}()
}
