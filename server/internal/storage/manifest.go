package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ManifestFile describes one backed-up file: its path (relative to the
// watched root), metadata used for the agent's fast unchanged-file skip,
// and the list of content-addressed chunk hashes that make up its bytes
// (a single entry for files smaller than the chunk size).
type ManifestFile struct {
	Path    string   `json:"path"`
	Size    int64    `json:"size"`
	ModTime int64    `json:"mtime"`
	SHA256  string   `json:"sha256"`
	Chunks  []string `json:"chunks"`
}

type Manifest struct {
	DeviceID   string         `json:"device_id"`
	SnapshotID string         `json:"snapshot_id"`
	CreatedAt  string         `json:"created_at"`
	Files      []ManifestFile `json:"files"`
}

func (s *Store) ManifestPath(deviceID, snapshotID string) string {
	return filepath.Join(s.Root, "manifests", deviceID, snapshotID+".json")
}

func (s *Store) WriteManifest(m *Manifest) (string, error) {
	path := s.ManifestPath(m.DeviceID, m.SnapshotID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func DeleteManifest(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
