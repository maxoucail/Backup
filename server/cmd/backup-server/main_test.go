package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"backup-server/internal/auth"
	"backup-server/internal/db"
	"backup-server/internal/models"
	"backup-server/internal/ws"
)

// offerRescheduleIfOverdue is what turns a reconnect into a reschedule
// popup on the agent: it must fire TypeOfferReschedule when the device's
// last successful backup is older than its effective interval, and stay
// silent otherwise (a device that has never backed up, or is simply not
// due yet, has nothing to catch up on).
func dialAgent(t *testing.T, hub *ws.Hub, deviceID string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeAgent(w, r, deviceID, "127.0.0.1")
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readOneEnvelope(t *testing.T, conn *websocket.Conn) (ws.Envelope, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return ws.Envelope{}, false
	}
	var env ws.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return env, true
}

func TestOfferRescheduleIfOverdueSendsCommandWhenPastDue(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close()
	if err := db.Bootstrap(sqlDB, t.TempDir(), t.TempDir(), auth.HashPassword); err != nil {
		t.Fatalf("db.Bootstrap: %v", err)
	}

	device, err := models.CreateDevice(sqlDB, "PC-Test", "pc-test", "Windows", "11", "1.0.0", "hash", "10.0.0.5")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	settings, err := models.GetSettings(sqlDB)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	settings.DefaultIntervalMinutes = 60
	if err := models.UpdateSettings(sqlDB, settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// A successful backup started two hours ago is well past a one-hour
	// interval - the device is overdue.
	if _, err := sqlDB.Exec(
		`INSERT INTO snapshots (id, device_id, kind, status, started_at) VALUES ('snap1', ?, 'scheduled', 'success', datetime('now', '-2 hours'))`,
		device.ID,
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	hub := ws.NewHub(sqlDB)
	conn := dialAgent(t, hub, device.ID)

	offerRescheduleIfOverdue(sqlDB, hub, device.ID)

	env, ok := readOneEnvelope(t, conn)
	if !ok {
		t.Fatal("aucun message reçu, attendu offer_reschedule")
	}
	if env.Type != ws.TypeOfferReschedule {
		t.Fatalf("type reçu = %q, attendu %q", env.Type, ws.TypeOfferReschedule)
	}
}

func TestOfferRescheduleIfOverdueStaysSilentWhenNotDueYet(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close()
	if err := db.Bootstrap(sqlDB, t.TempDir(), t.TempDir(), auth.HashPassword); err != nil {
		t.Fatalf("db.Bootstrap: %v", err)
	}

	device, err := models.CreateDevice(sqlDB, "PC-Test", "pc-test", "Windows", "11", "1.0.0", "hash", "10.0.0.5")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	settings, err := models.GetSettings(sqlDB)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	settings.DefaultIntervalMinutes = 60
	if err := models.UpdateSettings(sqlDB, settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// Started five minutes ago against a one-hour interval: not due yet.
	if _, err := sqlDB.Exec(
		`INSERT INTO snapshots (id, device_id, kind, status, started_at) VALUES ('snap1', ?, 'scheduled', 'success', datetime('now', '-5 minutes'))`,
		device.ID,
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	hub := ws.NewHub(sqlDB)
	conn := dialAgent(t, hub, device.ID)

	offerRescheduleIfOverdue(sqlDB, hub, device.ID)

	if env, ok := readOneEnvelope(t, conn); ok {
		t.Fatalf("message inattendu reçu: %+v", env)
	}
}

func TestOfferRescheduleIfOverdueStaysSilentWithoutAnySuccessfulBackup(t *testing.T) {
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close()
	if err := db.Bootstrap(sqlDB, t.TempDir(), t.TempDir(), auth.HashPassword); err != nil {
		t.Fatalf("db.Bootstrap: %v", err)
	}

	device, err := models.CreateDevice(sqlDB, "PC-Test", "pc-test", "Windows", "11", "1.0.0", "hash", "10.0.0.5")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	hub := ws.NewHub(sqlDB)
	conn := dialAgent(t, hub, device.ID)

	offerRescheduleIfOverdue(sqlDB, hub, device.ID)

	if env, ok := readOneEnvelope(t, conn); ok {
		t.Fatalf("message inattendu reçu: %+v", env)
	}
}
