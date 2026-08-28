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

	// lastProgressWrite throttles snapshot-progress persistence; only
	// touched from readPump's goroutine, so it needs no lock.
	lastProgressWrite time.Time
}

// progressWriteInterval bounds how often a device's progress is persisted.
// Agents report progress after every uploaded chunk, which for a large
// backup is thousands of messages - and with several devices backing up at
// once that turns into a steady stream of UPDATEs competing for SQLite's
// single writer. The panel polls every few seconds anyway, so persisting
// more often than this buys nothing.
const progressWriteInterval = 2 * time.Second

// Hub tracks every currently-connected agent so the panel can send it
// commands and know whether it's online. One goroutine pair (reader +
// writer) per connection; thousands of idle agents cost only a few KB each.
type Hub struct {
	db *sql.DB

	mu    sync.RWMutex
	conns map[string]*agentConn

	// OnDisconnect, when set, is called after a device's connection is
	// torn down. The queue uses it to reclaim the backup slot of a machine
	// that vanished mid-backup, which would otherwise hold the whole fleet
	// up until the stale sweep noticed.
	OnDisconnect func(deviceID string)
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
		// (agent reconnected after a network blip). Close the socket and
		// let the old pumps unwind on their own: closing old.send here
		// instead would race SendCommand, which by design does its channel
		// write after releasing h.mu - and a send on a closed channel is
		// an unrecoverable panic that would take the whole server down.
		// Closing the conn makes the old readPump fail, which closes its
		// done channel, which stops the old writePump.
		_ = old.conn.Close()
	}
	h.conns[deviceID] = ac
	h.mu.Unlock()

	_ = models.UpdateDeviceSeen(h.db, deviceID, "online", remoteIP)
	_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Agent connecté.")

	done := make(chan struct{})
	go h.writePump(ac, done)
	h.readPump(ac, done)

	// This teardown may be a connection that has already been superseded by
	// a reconnect (the replaced one only unwinds after the new one is
	// registered). In that case the device is very much still online and
	// possibly mid-backup: recording it offline would flip the panel to a
	// wrong state, and releasing its queue slot would let a second machine
	// start alongside it. Only the current connection's teardown counts.
	h.mu.Lock()
	current := h.conns[deviceID] == ac
	if current {
		delete(h.conns, deviceID)
	}
	h.mu.Unlock()

	if !current {
		return
	}

	_ = models.SetDeviceStatus(h.db, deviceID, "offline")
	_ = models.AddEvent(h.db, &deviceID, models.EventLevelInfo, "Agent déconnecté.")

	if h.OnDisconnect != nil {
		h.OnDisconnect(deviceID)
	}
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
		h.handleIncoming(ac, env)
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
		// ac.send is deliberately never closed (see ServeAgent): this pump
		// stops via done or a failed write, never via a channel close.
		case env := <-ac.send:
			_ = ac.conn.SetWriteDeadline(time.Now().Add(writeWait))
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

func (h *Hub) handleIncoming(ac *agentConn, env Envelope) {
	deviceID := ac.deviceID
	switch env.Type {
	case TypeHello:
		_, _ = h.db.Exec(`UPDATE devices SET os_name=?, os_version=?, agent_version=?, hostname=? WHERE id=?`,
			env.OSName, env.OSVersion, env.AgentVersion, env.Hostname, deviceID)

	case TypeProgress:
		// Persist at most one progress update per interval per device, but
		// never drop a terminal one - the panel would otherwise show a
		// backup stuck at whatever percentage the last throttled write
		// happened to catch.
		if env.SnapshotID != "" && (env.Percent >= 100 || time.Since(ac.lastProgressWrite) >= progressWriteInterval) {
			ac.lastProgressWrite = time.Now()
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
	}
}
