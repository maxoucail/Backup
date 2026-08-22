package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type downloadFile struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Label string `json:"label"`
}

// friendlyLabel gives the panel something nicer to show than a raw
// filename, guessed from common naming patterns so an operator who
// uploads "BackupAgentSetup.exe" or "BackupAgent-macOS-1.2.0.tar.gz"
// gets a sensible platform label for free.
func friendlyLabel(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".exe"):
		return "Windows"
	case strings.Contains(lower, "macos") || strings.HasSuffix(lower, ".pkg") || strings.HasSuffix(lower, ".dmg"):
		return "macOS"
	default:
		return "Fichier"
	}
}

// handleListDownloads is intentionally unauthenticated: it's the list of
// installers an employee needs to grab to set up their own machine, and
// they don't have (and shouldn't need) an admin login for that.
func (a *API) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(a.DownloadsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, []downloadFile{})
		return
	}
	files := make([]downloadFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, downloadFile{Name: e.Name(), Size: info.Size(), Label: friendlyLabel(e.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	writeJSON(w, http.StatusOK, files)
}

// handleDownloadFile serves one file from the downloads directory, also
// unauthenticated for the same reason. filepath.Base strips any path
// components a malicious filename might carry, and the resulting path is
// re-checked to stay inside DownloadsDir before ever touching the
// filesystem - the standard guard against path traversal.
func (a *API) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if name == "." || name == "/" || name == "" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(a.DownloadsDir, name)
	if !strings.HasPrefix(full, filepath.Clean(a.DownloadsDir)+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fileModTime(full), f)
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

const maxUploadBytes = 512 * 1024 * 1024 // installers are a few MB; generous ceiling against abuse

func (a *API) handleUploadDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "fichier trop volumineux ou requête invalide")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "aucun fichier reçu")
		return
	}
	defer file.Close()

	name := filepath.Base(header.Filename)
	if name == "" || name == "." || strings.HasPrefix(name, ".") {
		writeError(w, http.StatusBadRequest, "nom de fichier invalide")
		return
	}

	dest := filepath.Join(a.DownloadsDir, name)
	tmp := dest + ".uploading"
	out, err := os.Create(tmp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "échec de l'écriture")
		return
	}
	out.Close()
	if err := os.Rename(tmp, dest); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *API) handleDeleteDownload(w http.ResponseWriter, r *http.Request, name string) {
	name = filepath.Base(name)
	full := filepath.Join(a.DownloadsDir, name)
	if !strings.HasPrefix(full, filepath.Clean(a.DownloadsDir)+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest, "nom de fichier invalide")
		return
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
