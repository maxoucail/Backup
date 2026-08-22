// Package config handles the small amount of bootstrap configuration the
// server needs before it can open its database: where its data directory
// lives and the secret used to sign session cookies.
//
// Everything an operator changes after installation (storage location,
// backup interval, retention count, event-log retention) lives in the
// database instead (see models.Settings) and is edited from the web panel.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

type Config struct {
	DataDir       string
	DBPath        string
	StorageRoot   string
	ListenHost    string
	ListenPort    string
	SessionSecret string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads bootstrap configuration from the environment, creating the
// data directory and a persistent session secret on first run.
func Load() (*Config, error) {
	dataDir := envOr("BACKUP_SERVER_DATA_DIR", "/var/lib/backup-server")
	storageRoot := envOr("BACKUP_SERVER_STORAGE_ROOT", filepath.Join(dataDir, "storage"))

	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(storageRoot, 0o750); err != nil {
		return nil, err
	}

	secret, err := loadOrCreateSecret(filepath.Join(dataDir, "session.secret"))
	if err != nil {
		return nil, err
	}

	return &Config{
		DataDir:       dataDir,
		DBPath:        filepath.Join(dataDir, "server.db"),
		StorageRoot:   storageRoot,
		ListenHost:    envOr("BACKUP_SERVER_HOST", "0.0.0.0"),
		ListenPort:    envOr("BACKUP_SERVER_PORT", "8420"),
		SessionSecret: secret,
	}, nil
}

func loadOrCreateSecret(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return string(data), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}
