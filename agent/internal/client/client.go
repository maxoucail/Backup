// Package client talks to the backup server: enrollment, the chunk/manifest
// data plane over HTTP, and the command/progress control plane over
// WebSocket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	req.Header.Set("Content-Type", "application/json")
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
	ChunkSizeBytes  int64    `json:"chunk_size_bytes"`
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

func (c *Client) CreateSnapshot(ctx context.Context, kind string) (string, error) {
	body, _ := json.Marshal(map[string]string{"kind": kind})
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var out struct {
		SnapshotID string `json:"snapshot_id"`
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if err := doJSON(client, req, &out); err != nil {
		return "", err
	}
	return out.SnapshotID, nil
}

func (c *Client) CheckChunks(ctx context.Context, snapshotID string, hashes []string) ([]string, error) {
	body, _ := json.Marshal(map[string][]string{"hashes": hashes})
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots/"+snapshotID+"/check-chunks", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out struct {
		Missing []string `json:"missing"`
	}
	client := &http.Client{Timeout: 60 * time.Second}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	return out.Missing, nil
}

func (c *Client) UploadChunk(ctx context.Context, hash string, r io.Reader, size int64) error {
	req, err := c.authRequest(ctx, http.MethodPut, "/api/agent/chunks/"+hash, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	client := &http.Client{Timeout: 5 * time.Minute}
	return doJSON(client, req, nil)
}

func (c *Client) SubmitManifest(ctx context.Context, snapshotID string, manifest *protocol.Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	req, err := c.authRequest(ctx, http.MethodPost, "/api/agent/snapshots/"+snapshotID+"/manifest", bytes.NewReader(data))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	return doJSON(client, req, nil)
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

func (c *Client) GetManifest(ctx context.Context, snapshotID string) (*protocol.Manifest, error) {
	req, err := c.authRequest(ctx, http.MethodGet, "/api/agent/snapshots/"+snapshotID+"/manifest", nil)
	if err != nil {
		return nil, err
	}
	var out protocol.Manifest
	client := &http.Client{Timeout: 30 * time.Second}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DownloadChunk(ctx context.Context, hash string) (io.ReadCloser, error) {
	req, err := c.authRequest(ctx, http.MethodGet, "/api/agent/chunks/"+hash, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("téléchargement du chunk %s: %d %s", hash, resp.StatusCode, string(data))
	}
	return resp.Body, nil
}
