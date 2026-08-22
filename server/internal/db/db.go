// Package db owns the SQLite connection and schema. Plain SQL is used
// throughout the project instead of an ORM: the schema is small, and hand
// written queries keep the hot paths (chunk bookkeeping, event inserts
// under concurrent agent load) predictable and easy to reason about.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"backup-server/internal/idgen"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	storage_root TEXT NOT NULL,
	default_interval_minutes INTEGER NOT NULL DEFAULT 360,
	default_retention_count INTEGER NOT NULL DEFAULT 7,
	event_retention_days INTEGER NOT NULL DEFAULT 30,
	event_retention_max_rows INTEGER NOT NULL DEFAULT 20000,
	chunk_size_bytes INTEGER NOT NULL DEFAULT 16777216
);

CREATE TABLE IF NOT EXISTS enrollment_keys (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL,
	expires_at DATETIME NOT NULL,
	used_at DATETIME,
	used_by_device_id TEXT
);

CREATE TABLE IF NOT EXISTS devices (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	hostname TEXT NOT NULL DEFAULT '',
	os_name TEXT NOT NULL DEFAULT '',
	os_version TEXT NOT NULL DEFAULT '',
	agent_version TEXT NOT NULL DEFAULT '',
	secret_hash TEXT NOT NULL,
	ip_address TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'offline',
	last_seen DATETIME,
	created_at DATETIME NOT NULL,
	interval_minutes INTEGER,
	retention_count INTEGER,
	backup_paths TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS snapshots (
	id TEXT PRIMARY KEY,
	device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	kind TEXT NOT NULL DEFAULT 'scheduled',
	status TEXT NOT NULL DEFAULT 'running',
	started_at DATETIME NOT NULL,
	finished_at DATETIME,
	file_count INTEGER NOT NULL DEFAULT 0,
	logical_bytes INTEGER NOT NULL DEFAULT 0,
	uploaded_bytes INTEGER NOT NULL DEFAULT 0,
	progress_percent REAL NOT NULL DEFAULT 0,
	error_message TEXT NOT NULL DEFAULT '',
	manifest_path TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_snapshots_device ON snapshots(device_id, started_at);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id TEXT REFERENCES devices(id) ON DELETE CASCADE,
	ts DATETIME NOT NULL,
	level TEXT NOT NULL DEFAULT 'info',
	message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_device ON events(device_id, ts);
`

// Open opens (creating if necessary) the SQLite database and applies the
// schema. WAL mode lets the API handlers, the WebSocket agent hub, and the
// background scheduler read and write concurrently without lock
// contention, which matters once several devices are backing up at once.
func Open(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; keep the pool modest so we don't
	// pile up goroutines waiting behind SQLITE_BUSY beyond the busy_timeout.
	sqlDB.SetMaxOpenConns(8)

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return sqlDB, nil
}

// Bootstrap seeds the settings row and, if no admin exists yet, creates a
// default "admin" account with a random password that's printed once and
// saved next to the database so the operator can log in and change it.
func Bootstrap(sqlDB *sql.DB, dataDir, storageRoot string, hashPassword func(string) (string, error)) error {
	var count int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := sqlDB.Exec(`INSERT INTO settings (id, storage_root) VALUES (1, ?)`, storageRoot)
		if err != nil {
			return err
		}
	}

	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		password, err := randomPassword()
		if err != nil {
			return err
		}
		hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		id := idgen.New()
		_, err = sqlDB.Exec(
			`INSERT INTO users (id, username, password_hash, created_at, updated_at) VALUES (?, 'admin', ?, datetime('now'), datetime('now'))`,
			id, hash,
		)
		if err != nil {
			return err
		}

		msg := fmt.Sprintf(
			"\n============================================================\n"+
				" Compte administrateur initial cree :\n"+
				"   utilisateur : admin\n"+
				"   mot de passe : %s\n"+
				" Changez ce mot de passe depuis le panneau apres connexion.\n"+
				" (aussi enregistre dans %s)\n"+
				"============================================================\n",
			password, filepath.Join(dataDir, "initial_admin_password.txt"),
		)
		log.Print(msg)
		_ = os.WriteFile(filepath.Join(dataDir, "initial_admin_password.txt"), []byte(password+"\n"), 0o600)
	}
	return nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
