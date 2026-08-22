package client

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"backup-agent/internal/protocol"
)

// WSClient maintains the persistent control-plane connection to the
// server: it reconnects with backoff on any failure so the agent stays
// reachable for remote commands even across a NAS reboot or a flaky
// network, without needing anything smarter than "keep trying".
type WSClient struct {
	ServerURL    string
	DeviceID     string
	DeviceSecret string
	HelloInfo    protocol.Envelope

	Incoming chan protocol.Envelope
	// Unauthorized is signalled (non-blocking) when the server rejects this
	// device's credentials outright - see client.ErrUnauthorized.
	Unauthorized chan struct{}
	outgoing     chan protocol.Envelope

	connectedMu sync.RWMutex
	connected   bool
}

func NewWSClient(serverURL, deviceID, deviceSecret string, hello protocol.Envelope) *WSClient {
	return &WSClient{
		ServerURL:    serverURL,
		DeviceID:     deviceID,
		DeviceSecret: deviceSecret,
		HelloInfo:    hello,
		Incoming:     make(chan protocol.Envelope, 32),
		Unauthorized: make(chan struct{}, 1),
		outgoing:     make(chan protocol.Envelope, 128),
	}
}

func (w *WSClient) Send(env protocol.Envelope) {
	select {
	case w.outgoing <- env:
	default:
		log.Printf("ws: outgoing buffer full, dropping %s message", env.Type)
	}
}

func (w *WSClient) Connected() bool {
	w.connectedMu.RLock()
	defer w.connectedMu.RUnlock()
	return w.connected
}

func (w *WSClient) setConnected(v bool) {
	w.connectedMu.Lock()
	w.connected = v
	w.connectedMu.Unlock()
}

func wsURL(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/agent"
	return u.String(), nil
}

// Run blocks until ctx is cancelled, maintaining the connection and
// reconnecting with capped exponential backoff on any failure.
func (w *WSClient) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.runOnce(ctx); err != nil {
			log.Printf("ws: connexion perdue: %v (nouvelle tentative dans %s)", err, backoff)
		}
		w.setConnected(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (w *WSClient) runOnce(ctx context.Context) error {
	target, err := wsURL(w.ServerURL)
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("X-Device-Id", w.DeviceID)
	header.Set("X-Device-Secret", w.DeviceSecret)

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, target, header)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			select {
			case w.Unauthorized <- struct{}{}:
			default:
			}
		}
		return err
	}
	defer conn.Close()

	w.setConnected(true)
	log.Printf("ws: connecté à %s", target)
	w.Send(w.HelloInfo)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go w.writeLoop(connCtx, conn)

	conn.SetReadLimit(1 << 20)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("ws: message illisible: %v", err)
			continue
		}
		select {
		case w.Incoming <- env:
		case <-connCtx.Done():
			return connCtx.Err()
		}
	}
}

func (w *WSClient) writeLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-w.outgoing:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
