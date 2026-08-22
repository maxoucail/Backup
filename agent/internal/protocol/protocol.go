// Package protocol mirrors the wire types defined by the server
// (backup-server/internal/storage.Manifest and backup-server/internal/ws.Envelope).
// The two codebases are separate Go modules by design - the agent must
// build and run without ever needing the server's source - so the JSON
// shapes are kept in sync here by contract rather than by a shared import.
package protocol

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

// Envelope is the WebSocket control-plane message shape, identical to
// backup-server/internal/ws.Envelope.
type Envelope struct {
	Type string `json:"type"`

	Hostname     string `json:"hostname,omitempty"`
	OSName       string `json:"os_name,omitempty"`
	OSVersion    string `json:"os_version,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`

	SnapshotID string `json:"snapshot_id,omitempty"`

	Phase         string  `json:"phase,omitempty"`
	FileCount     int     `json:"file_count,omitempty"`
	LogicalBytes  int64   `json:"logical_bytes,omitempty"`
	UploadedBytes int64   `json:"uploaded_bytes,omitempty"`
	Percent       float64 `json:"percent,omitempty"`
	EtaSeconds    int64   `json:"eta_seconds,omitempty"`

	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`

	Status       string `json:"status,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	IntervalMinutes *int     `json:"interval_minutes,omitempty"`
	RetentionCount  *int     `json:"retention_count,omitempty"`
	BackupPaths     []string `json:"backup_paths,omitempty"`
}

const (
	TypeHello           = "hello"
	TypeConfig          = "config"
	TypeBackupNow       = "backup_now"
	TypeRestore         = "restore"
	TypeCancel          = "cancel"
	TypeUninstall       = "uninstall"
	TypeProgress        = "progress"
	TypeLog             = "log"
	TypeBackupStarted   = "backup_started"
	TypeBackupFinished  = "backup_finished"
	TypeRestoreStarted  = "restore_started"
	TypeRestoreFinished = "restore_finished"
)

const (
	SnapshotStatusSuccess   = "success"
	SnapshotStatusFailed    = "failed"
	SnapshotStatusCancelled = "cancelled"

	SnapshotKindScheduled = "scheduled"
	SnapshotKindManual    = "manual"

	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
)
