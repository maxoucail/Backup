// Package protocol mirrors the wire types defined by the server
// (backup-server/internal/filestore.FileInfo and
// backup-server/internal/ws.Envelope). The two codebases are separate Go
// modules by design - the agent must build and run without ever needing
// the server's source - so the JSON shapes are kept in sync here by
// contract rather than by a shared import.
package protocol

// FileInfo is one entry of what this machine currently holds, as announced
// to the server's plan endpoint. Size and mtime are what the server
// compares against its own record to decide whether the file needs
// sending - no hashing on either side, which is what keeps a scan of a
// large disk cheap. ModTime is nanoseconds since epoch, read straight off
// the local file at the moment it's opened for upload - full precision,
// not truncated to the second - since the server's comparison depends on
// it to tell apart a genuine edit from one that just happens to land in
// the same second as a previous read.
type FileInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
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
	TypeHello          = "hello"
	TypeConfig         = "config"
	TypeBackupNow      = "backup_now"
	TypeCancel         = "cancel"
	TypeUninstall      = "uninstall"
	TypeProgress       = "progress"
	TypeLog            = "log"
	TypeBackupStarted  = "backup_started"
	TypeBackupFinished = "backup_finished"
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
