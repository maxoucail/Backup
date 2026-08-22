// Package web serves the browser-facing panel: a handful of static HTML
// shells (each a self-contained page that fetches its data from the JSON
// API) plus their CSS/JS assets. Everything is embedded into the compiled
// binary via embed.FS, so deployment is copying one file - no separate
// asset directory that could go missing or drift from the binary.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"backup-server/internal/auth"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

type Web struct {
	Sessions  *auth.SessionSigner
	templates map[string]*template.Template
}

func New(sessions *auth.SessionSigner) (*Web, error) {
	names := []string{"login.html", "dashboard.html", "devices.html", "device.html", "settings.html"}
	templates := make(map[string]*template.Template, len(names))
	for _, name := range names {
		t, err := template.ParseFS(templatesFS, "templates/"+name)
		if err != nil {
			return nil, err
		}
		templates[name] = t
	}
	return &Web{Sessions: sessions, templates: templates}, nil
}

func (w2 *Web) render(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := w2.templates[name].Execute(w, nil); err != nil {
		http.Error(w, "erreur de rendu", http.StatusInternalServerError)
	}
}

func (w2 *Web) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie("backup_session")
	if err != nil {
		return false
	}
	_, err = w2.Sessions.ReadCookie(cookie.Value)
	return err == nil
}

func (w2 *Web) Register(mux *http.ServeMux) {
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if w2.isAuthenticated(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w2.render(w, "login.html")
	})

	protected := map[string]string{
		"/":         "dashboard.html",
		"/devices":  "devices.html",
		"/settings": "settings.html",
	}
	for path, tmpl := range protected {
		path, tmpl := path, tmpl
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if !w2.isAuthenticated(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			w2.render(w, tmpl)
		})
	}

	mux.HandleFunc("GET /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !w2.isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w2.render(w, "device.html")
	})
}
