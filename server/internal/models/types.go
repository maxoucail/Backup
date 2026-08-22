// Package models is the repository layer: typed structs plus plain-SQL
// accessors for every table. Kept deliberately free of any HTTP or
// WebSocket concerns so it can be unit tested and reused by both the REST
// API and the agent WebSocket hub.
package models

import "time"

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Settings struct {
	StorageRoot            string `json:"storage_root"`
	DefaultIntervalMinutes int    `json:"default_interval_minutes"`
	DefaultRetentionCount  int    `json:"default_retention_count"`
	EventRetentionDays     int    `json:"event_retention_days"`
	EventRetentionMaxRows  int    `json:"event_retention_max_rows"`
	ChunkSizeBytes         int64  `json:"chunk_size_bytes"`
}

type EnrollmentKey struct {
	ID             string     `json:"id"`
	TokenHash      string     `json:"-"`
	Label          string     `json:"label"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	UsedAt         *time.Time `json:"used_at,omitempty"`
	UsedByDeviceID *string    `json:"used_by_device_id,omitempty"`
}

type Device struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Hostname        string     `json:"hostname"`
	OSName          string     `json:"os_name"`
	OSVersion       string     `json:"os_version"`
	AgentVersion    string     `json:"agent_version"`
	SecretHash      string     `json:"-"`
	IPAddress       string     `json:"ip_address"`
	Status          string     `json:"status"` // online / offline
	LastSeen        *time.Time `json:"last_seen,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	IntervalMinutes *int       `json:"interval_minutes,omitempty"`
	RetentionCount  *int       `json:"retention_count,omitempty"`
	BackupPaths     string     `json:"backup_paths,omitempty"` // JSON list; "" = agent defaults
}

const (
	SnapshotStatusRunning   = "running"
	SnapshotStatusSuccess   = "success"
	SnapshotStatusFailed    = "failed"
	SnapshotStatusCancelled = "cancelled"

	SnapshotKindScheduled = "scheduled"
	SnapshotKindManual    = "manual"
)

type Snapshot struct {
	ID              string     `json:"id"`
	DeviceID        string     `json:"device_id"`
	Kind            string     `json:"kind"`
	Status          string     `json:"status"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	FileCount       int        `json:"file_count"`
	LogicalBytes    int64      `json:"logical_bytes"`
	UploadedBytes   int64      `json:"uploaded_bytes"`
	ProgressPercent float64    `json:"progress_percent"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	ManifestPath    string     `json:"-"`
}

const (
	EventLevelInfo    = "info"
	EventLevelWarning = "warning"
	EventLevelError   = "error"
)

type Event struct {
	ID       int64     `json:"id"`
	DeviceID *string   `json:"device_id,omitempty"`
	TS       time.Time `json:"ts"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
}
