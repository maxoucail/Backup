// Package client talks to the backup server: enrollment, the file data
// plane over HTTP (announce what this machine holds, upload what the
// server asks for), and the command/progress control plane over WebSocket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"backup-agent/internal/protocol"
)

// ErrUnauthorized means the server rejected this device's credentials
// outright (401) - not a transient network problem. In practice this
// means an operator decommissioned/deleted this device from the panel;
// the agent's enrollment is dead and it needs to re-run the setup wizard
// rather than retry forever.
var ErrUnauthorized = errors.New("appareil non reconnu par le serveur (décommissionné ?)")

// uploadClient is dedicated to UploadFile. Keep-alives were briefly
// disabled here entirely: a large upload that died mid-transfer could
// leave its TCP connection in an indeterminate state, and a later request
// reusing it read back as a nonsensical error (a stray 405) unrelated to
// that request. The real culprit behind those mid-transfer deaths turned
// out to be macOS's sendfile() (see noReaderFrom below), not connection
// reuse itself - now that uploads never take that path, there's nothing
// left routinely leaving a connection in a bad state, and paying for a
// fresh TCP handshake (plus, on a routed/VPN path, its higher RTT) for
// every single one of what can be tens of thousands of small files was
// most of what made backups feel slow. MaxIdleConnsPerHost is raised well
// past Go's default of 2 so every one of uploadConcurrency's concurrent
// workers actually gets to keep its own connection warm between files,
// instead of most of them still opening fresh ones because the pool was
// too small to hold them all.
var uploadClient = &http.Client{
	Timeout:   30 * time.Minute,
	Transport: &http.Transport{MaxIdleConnsPerHost: 32},
}

type Client struct {
	ServerURL    string // e.g. http://192.168.1.10:8420
	DeviceID     string
	DeviceSecret string
	HTTP         *http.Client
}

func New(serverURL, deviceID, deviceSecret string) *Client {
	return &Client{
		ServerURL:    strings.TrimRight(serverURL, "/"),
		DeviceID:     deviceID,
		DeviceSecret: deviceSecret,
		HTTP:         &http.Client{},
	}
}

func (c *Client) authRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.ServerURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Device-Id", c.DeviceID)
	req.Header.Set("X-Device-Secret", c.DeviceSecret)
	return req, nil
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Enroll exchanges a one-time enrollment key for a permanent device
// identity. Unauthenticated - this is the one call made before the agent
// has any credentials at all.
func Enroll(ctx context.Context, serverURL, token, name, hostname, osName, osVersion, agentVersion string) (deviceID, deviceSecret string, err error) {
	body, _ := json.Marshal(map[string]string{
		"token": token, "name": name, "hostname": hostname,
		"os_name": osName, "os_version": osVersion, "agent_version": agentVersion,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	var out struct {
		DeviceID     string `json:"device_id"`
		DeviceSecret string `json:"device_secret"`
	}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	if err := doJSON(httpClient, req, &out); err != nil {
		return "", "", err
	}
	return out.DeviceID, out.DeviceSecret, nil
}

type PolicyResponse struct {
	IntervalMinutes int      `json:"interval_minutes"`
	RetentionCount  int      `json:"retention_count"`
	BackupPaths     []string `json:"backup_paths"`
}

func (c *Client) GetConfig(ctx context.Context) (*PolicyResponse, error) {
	req, err := c.authRequest(ctx, http.MethodGet, "/api/agent/config", nil)
	if err != nil {
		return nil, err
	}
	var out PolicyResponse
	client := &http.Client{Timeout: 20 * time.Second}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrQueued means the server accepted the request but there's no free
// backup slot: another machine is backing up. The agent must wait for the
// server to call it back rather than starting anyway - see internal/queue
// on the server side. It is deliberately not an error condition for the
// user; nothing failed.
var ErrQueued = errors.New("sauvegarde mise en file d'attente par le serveur")

// QueuePosition carries how many devices are ahead, for logging.
type QueuedError struct {
	Position int
}

func (e *QueuedError) Error() string {
	return fmt.Sprintf("%s (position %d)", ErrQueued.Error(), e.Position)
}
func (e *QueuedError) Is(target error) bool { return target == ErrQueued }

func (c *Client) CreateSnapshot(ctx context.Context, kind string) (string, error) {
	body, _ := json.Marshal(map[string]string{"kind": kind})
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var out struct {
		SnapshotID string `json:"snapshot_id"`
		Queued     bool   `json:"queued"`
		Position   int    `json:"position"`
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if err := doJSON(client, req, &out); err != nil {
		return "", err
	}
	if out.Queued {
		return "", &QueuedError{Position: out.Position}
	}
	return out.SnapshotID, nil
}

// Plan announces everything this machine currently holds and gets back
// just the subset the server doesn't already have an identical copy of.
//
// This is what makes the backup incremental: the server compares the list
// against the files already sitting in this machine's folder on the NAS,
// so an unchanged 4 GB photo library is never re-sent. It also gives the
// server the moment to preserve the current state as a dated version
// before anything is overwritten.
func (c *Client) Plan(ctx context.Context, snapshotID string, files []protocol.FileInfo) (needed []string, destination string, err error) {
	body, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return nil, "", err
	}
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots/"+snapshotID+"/plan", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Needed      []string `json:"needed"`
		Destination string   `json:"destination"`
	}
	// A first backup of a large disk can announce hundreds of thousands of
	// files, and the server preserves the previous version before replying,
	// so this one call is legitimately slow.
	client := &http.Client{Timeout: 30 * time.Minute}
	if err := doJSON(client, req, &out); err != nil {
		return nil, "", err
	}
	return out.Needed, out.Destination, nil
}

// noReaderFrom hides an io.Reader's concrete type behind a plain
// interface value, keeping only Read. Go's net.TCPConn.ReadFrom
// recognizes an *os.File source and hands it to the kernel's sendfile()
// for a zero-copy send; on Linux and Windows that's a reliable win, but
// Darwin's sendfile() has long-standing kernel bugs under concurrent use
// that surface to callers as exactly the errors macOS reported here -
// "sendfile: broken pipe", "sendfile: socket is not connected" - both are
// Go's own internal/poll wrapper naming the syscall it used, not this
// agent's own error text, and not a symptom of the network path. Wrapping
// the reader so its type is no longer *os.File defeats that type check
// and forces the ordinary buffered copy loop instead - a little less
// "zero-copy" efficient, immaterial next to the time spent transferring
// the file itself, and immune to the syscall this platform gets wrong.
type noReaderFrom struct{ io.Reader }

// UploadFile sends one file's raw bytes; the server writes it, in clear, at
// the same relative location under the machine's folder on the NAS.
// modTime is nanoseconds since epoch (see protocol.FileInfo).
func (c *Client) UploadFile(ctx context.Context, relPath string, modTime, size int64, r io.Reader) error {
	q := url.Values{}
	q.Set("path", relPath)
	q.Set("mtime", strconv.FormatInt(modTime, 10))
	req, err := c.authRequest(ctx, http.MethodPut, "/api/agent/files?"+q.Encode(), noReaderFrom{r})
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	return doJSON(uploadClient, req, nil)
}

// finishSnapshotTimeout used to be 30 seconds - fine for a small backup,
// but the server rewrites this device's whole file index before it can
// answer (see filestore.ConfirmUpdates), and for tens of thousands of
// files on a slow NAS that alone can take longer than that. The real bug
// this caused: the server would go on to actually finish and mark the
// snapshot successful, but the agent had already given up waiting and
// logged the run as failed - a backup that genuinely succeeded, showing
// up as an error in its own event history. Matches Plan's timeout for the
// same reason: a legitimately slow server-side step must not be mistaken
// for a dead connection.
const finishSnapshotTimeout = 30 * time.Minute

func (c *Client) FinishSnapshot(ctx context.Context, snapshotID, status, errMsg string, uploadedBytes int64) error {
	body, _ := json.Marshal(map[string]any{"status": status, "error_message": errMsg, "uploaded_bytes": uploadedBytes})
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots/"+snapshotID+"/finish", bytes.NewReader(body))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: finishSnapshotTimeout}
	return doJSON(client, req, nil)
}
