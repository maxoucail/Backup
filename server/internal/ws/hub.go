package ws

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"backup-server/internal/models"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Agents are not browsers; there's no third-party page that could be
	// tricked into opening this socket, so origin checking doesn't apply.
	CheckOrigin: func(r *http.Request) bool { return true },
}

type agentConn struct {
	deviceID string
	conn     *websocket.Conn
	send     chan Envelope
}

// Hub tracks every currently-connected agent so the panel can send it
// commands and know whether it's online. One goroutine pair (reader +
// writer) per connection; thousands of idle agents cost only a few KB each.
type Hub struct {
	db *sql.DB

	mu    sync.RWMutex
	conns map[string]*agentConn
}

func NewHub(db *sql.DB) *Hub {
	return &Hub{db: db, conns: make(map[string]*agentConn)}
}

func (h *Hub) IsOnline(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[deviceID]
	return ok
}

// SendCommand delivers a command to a connected agent. Returns false if the
// device isn't currently connected (caller should surface "device offline").
func (h *Hub) SendCommand(deviceID string, env Envelope) bool {
	h.mu.RLock()
	c, ok := h.conns[deviceID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case c.send <- env:
		return true
	default:
		log.Printf("ws: send buffer full for device %s, dropping command %s", deviceID, env.Type)
		return false
	}
}

// ServeAgent upgrades the connection and runs it until it closes. Device
// identity must already be validated by the caller (see api middleware);
// deviceID is trusted here.
func (h *Hub) ServeAgent(w http.ResponseWriter, r *http.Request, deviceID, remoteIP string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade failed for device %s: %v", deviceID, err)
		return
	}

	ac := &agentConn{deviceID: deviceID, conn: conn, send: make(chan Envelope, 32)}

	h.mu.Lock()
	if old, exists := h.conns[deviceID]; exists {
		// A previous connection for the same device is being replaced
		// (agent reconnected); close the stale one.
		close(old.send)
		_ = old.conn.Close()
	}
	h.conns[deviceID] = ac
	h.mu.Unlock()

	_ = models.UpdateDeviceSeen(h.db, deviceID, "online", remoteIP)
	_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Agent connecté.")

	done := make(chan struct{})
	go h.writePump(ac, done)
	h.readPump(ac, done)

	h.mu.Lock()
	if h.conns[deviceID] == ac {
		delete(h.conns, deviceID)
	}
	h.mu.Unlock()

	_ = models.SetDeviceStatus(h.db, deviceID, "offline")
	_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Agent déconnecté.")
}

func (h *Hub) readPump(ac *agentConn, done chan struct{}) {
	defer close(done)
	ac.conn.SetReadLimit(1 << 20)
	_ = ac.conn.SetReadDeadline(time.Now().Add(pongWait))
	ac.conn.SetPongHandler(func(string) error {
		_ = ac.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := ac.conn.ReadMessage()
		if err != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("ws: bad message from %s: %v", ac.deviceID, err)
			continue
		}
		h.handleIncoming(ac.deviceID, env)
	}
}

func (h *Hub) writePump(ac *agentConn, done chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = ac.conn.Close()
	}()

	for {
		select {
		case env, ok := <-ac.send:
			_ = ac.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = ac.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			if err := ac.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = ac.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := ac.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func (h *Hub) handleIncoming(deviceID string, env Envelope) {
	switch env.Type {
	case TypeHello:
		_, _ = h.db.Exec(`UPDATE devices SET os_name=?, os_version=?, agent_version=?, hostname=? WHERE id=?`,
			env.OSName, env.OSVersion, env.AgentVersion, env.Hostname, deviceID)

	case TypeProgress:
		if env.SnapshotID != "" {
			_ = models.UpdateSnapshotProgress(h.db, env.SnapshotID, env.FileCount, env.LogicalBytes, env.UploadedBytes, env.Percent)
		}

	case TypeLog:
		level := env.Level
		if level == "" {
			level = models.EventLevelInfo
		}
		_ = models.AddEvent(h.db, &deviceID, level, env.Message)

	case TypeBackupStarted:
		_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Sauvegarde démarrée.")

	case TypeBackupFinished:
		msg := "Sauvegarde terminée avec succès."
		level := models.EventLevelInfo
		if env.Status != models.SnapshotStatusSuccess {
			msg = "Sauvegarde en échec : " + env.ErrorMessage
			level = models.EventLevelError
		}
		_ = models.AddEvent(h.db, &deviceID, level, msg)

	case TypeRestoreStarted:
		_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Restauration démarrée.")

	case TypeRestoreFinished:
		msg := "Restauration terminée avec succès."
		level := models.EventLevelInfo
		if env.Status != models.SnapshotStatusSuccess {
			msg = "Restauration en échec : " + env.ErrorMessage
			level = models.EventLevelError
		}
		_ = models.AddEvent(h.db, &deviceID, level, msg)
	}
}
