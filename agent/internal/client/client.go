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

// uploadClient is dedicated to UploadFile and never reuses a connection
// across requests (DisableKeepAlives). Every other call on this package
// shares Go's default transport and its keep-alive pool, which is fine for
// small, quick requests - but a large file upload that dies mid-transfer
// (a network blip, a server restart) can leave that shared TCP connection
// in an indeterminate state; net/http is not always able to tell it apart
// from one that's merely idle before the next request tries to reuse it,
// and a request landing on a half-dead connection reads back as
// nonsensical errors (a stray 405 for a perfectly normal request) that
// have nothing to do with that request itself. Concurrent uploads for the
// same device make this worse purely by having more connections in the
// pool for one bad one to hide among. Uploads always paying for a fresh
// TCP handshake is a trivial cost next to transferring the file itself.
var uploadClient = &http.Client{
	Timeout:   30 * time.Minute,
	Transport: &http.Transport{DisableKeepAlives: true},
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

// UploadFile sends one file's raw bytes; the server writes it, in clear, at
// the same relative location under the machine's folder on the NAS.
// modTime is nanoseconds since epoch (see protocol.FileInfo).
func (c *Client) UploadFile(ctx context.Context, relPath string, modTime, size int64, r io.Reader) error {
	q := url.Values{}
	q.Set("path", relPath)
	q.Set("mtime", strconv.FormatInt(modTime, 10))
	req, err := c.authRequest(ctx, http.MethodPut, "/api/agent/files?"+q.Encode(), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	return doJSON(uploadClient, req, nil)
}

func (c *Client) FinishSnapshot(ctx context.Context, snapshotID, status, errMsg string, uploadedBytes int64) error {
	body, _ := json.Marshal(map[string]any{"status": status, "error_message": errMsg, "uploaded_bytes": uploadedBytes})
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots/"+snapshotID+"/finish", bytes.NewReader(body))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	return doJSON(client, req, nil)
}
