package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"backup-server/internal/db"
)

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	// A device row must exist: ServeAgent records presence against it.
	_, err = sqlDB.Exec(
		`INSERT INTO devices (id, name, secret_hash, created_at) VALUES ('dev1', 'test', 'x', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert device: %v", err)
	}
	return NewHub(sqlDB)
}

// An agent reconnecting (after a network blip, a laptop waking up, ...)
// replaces its previous connection in the hub. If that teardown races an
// operator clicking "Sauvegarder maintenant", the command must not be able
// to hit a torn-down connection in a way that crashes the process - a
// panic in any goroutine takes the whole backup server down with it.
func TestSendCommandDuringReconnectDoesNotPanic(t *testing.T) {
	hub := newTestHub(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeAgent(w, r, "dev1", "127.0.0.1")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Hammer the hub with commands, as several panel tabs would. The race
	// window between looking a connection up and writing to it is narrow,
	// so widen the odds with concurrent senders.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					hub.SendCommand("dev1", Envelope{Type: TypeBackupNow})
				}
			}
		}()
	}

	// Meanwhile the agent keeps reconnecting, each time displacing the
	// previous connection.
	for i := 0; i < 40; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
		_ = conn.Close()
	}

	close(stop)
	wg.Wait()
	// Reaching here without the process dying is the assertion: a send on a
	// closed channel would have panicked and failed the test binary.
}

// Progress arrives once per uploaded chunk - thousands of messages for a
// large backup, multiplied by every device backing up at the same time.
// Persisting each one would put SQLite's single writer under constant
// load, so writes are throttled; a completed backup must still land
// immediately, or the panel would show it stuck below 100%.
func TestProgressPersistenceIsThrottledButKeepsFinalUpdate(t *testing.T) {
	hub := newTestHub(t)
	if _, err := hub.db.Exec(
		`INSERT INTO snapshots (id, device_id, kind, status, started_at) VALUES ('snap1', 'dev1', 'manual', 'running', datetime('now'))`,
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	ac := &agentConn{deviceID: "dev1"}

	for i := 0; i < 50; i++ {
		hub.handleIncoming(ac, Envelope{Type: TypeProgress, SnapshotID: "snap1", Percent: 20})
	}

	var percent float64
	if err := hub.db.QueryRow(`SELECT progress_percent FROM snapshots WHERE id = 'snap1'`).Scan(&percent); err != nil {
		t.Fatalf("select: %v", err)
	}
	if percent != 20 {
		t.Fatalf("progression persistée = %v, attendu 20 (la première écriture doit passer)", percent)
	}

	// A burst of further updates within the throttle window must not each
	// hit the database; the recorded value stays at the last written one.
	for i := 0; i < 50; i++ {
		hub.handleIncoming(ac, Envelope{Type: TypeProgress, SnapshotID: "snap1", Percent: 55})
	}
	if err := hub.db.QueryRow(`SELECT progress_percent FROM snapshots WHERE id = 'snap1'`).Scan(&percent); err != nil {
		t.Fatalf("select: %v", err)
	}
	if percent != 20 {
		t.Fatalf("progression persistée = %v, attendu 20 (les mises à jour rapprochées doivent être filtrées)", percent)
	}

	// Completion bypasses the throttle.
	hub.handleIncoming(ac, Envelope{Type: TypeProgress, SnapshotID: "snap1", Percent: 100})
	if err := hub.db.QueryRow(`SELECT progress_percent FROM snapshots WHERE id = 'snap1'`).Scan(&percent); err != nil {
		t.Fatalf("select: %v", err)
	}
	if percent != 100 {
		t.Fatalf("progression finale = %v, attendu 100 (une progression terminale ne doit jamais être filtrée)", percent)
	}
}
