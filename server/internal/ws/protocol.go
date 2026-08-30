// Package ws is the agent control-plane: a persistent WebSocket per
// connected device used for presence, remote commands issued from the
// panel (backup now / cancel / policy push), and live progress/log
// streaming back to the server. Bulk data (file uploads) travels over
// plain HTTP instead - see internal/api - so this channel stays small and
// responsive even while a multi-gigabyte backup is in flight on the same
// connection's HTTP side.
package ws

// Envelope is the single message shape used in both directions. Only a
// subset of fields is populated depending on Type; unused fields are
// omitted from the wire format.
type Envelope struct {
	Type string `json:"type"`

	// agent -> server: hello
	Hostname     string `json:"hostname,omitempty"`
	OSName       string `json:"os_name,omitempty"`
	OSVersion    string `json:"os_version,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`

	// both directions
	SnapshotID string `json:"snapshot_id,omitempty"`

	// agent -> server: progress
	Phase         string  `json:"phase,omitempty"` // scanning/uploading/finalizing
	FileCount     int     `json:"file_count,omitempty"`
	LogicalBytes  int64   `json:"logical_bytes,omitempty"`
	UploadedBytes int64   `json:"uploaded_bytes,omitempty"`
	Percent       float64 `json:"percent,omitempty"`
	EtaSeconds    int64   `json:"eta_seconds,omitempty"`

	// agent -> server: log
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`

	// agent -> server: backup_finished
	Status       string `json:"status,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	// server -> agent: config
	IntervalMinutes *int     `json:"interval_minutes,omitempty"`
	RetentionCount  *int     `json:"retention_count,omitempty"`
	BackupPaths     []string `json:"backup_paths,omitempty"`
}

// Message types.
const (
	TypeHello           = "hello"            // agent -> server, on connect
	TypeConfig          = "config"           // server -> agent, policy push
	TypeBackupNow       = "backup_now"       // server -> agent, manual trigger
	TypeCancel          = "cancel"           // server -> agent, abort running job
	TypeUninstall       = "uninstall"        // server -> agent, decommission and self-remove
	TypeOfferReschedule = "offer_reschedule" // server -> agent, was overdue when it reconnected
	TypeProgress        = "progress"         // agent -> server
	TypeLog             = "log"              // agent -> server
	TypeBackupStarted   = "backup_started"   // agent -> server
	TypeBackupFinished  = "backup_finished"  // agent -> server
)
