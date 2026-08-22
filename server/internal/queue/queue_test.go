package queue

import (
	"sync"
	"testing"
	"time"
)

// recorder captures which devices the queue told to start, in order.
type recorder struct {
	mu      sync.Mutex
	started []string
	offline map[string]bool
}

func newRecorder() *recorder { return &recorder{offline: map[string]bool{}} }

func (r *recorder) dispatch(deviceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offline[deviceID] {
		return false
	}
	r.started = append(r.started, deviceID)
	return true
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...)
}

func fixedLimit(n int) MaxConcurrentFunc { return func() int { return n } }

// With the default limit of one, a second machine asking to back up must
// be told to wait rather than piling onto the same NAS and network link.
func TestSecondDeviceIsQueuedNotRunInParallel(t *testing.T) {
	rec := newRecorder()
	m := New(fixedLimit(1), rec.dispatch)

	if granted, _ := m.Acquire("pc-a"); !granted {
		t.Fatal("le premier appareil doit obtenir un créneau immédiatement")
	}
	granted, position := m.Acquire("pc-b")
	if granted {
		t.Fatal("le second appareil ne doit pas démarrer pendant que le premier sauvegarde")
	}
	if position != 1 {
		t.Fatalf("position en file = %d, attendu 1", position)
	}
}

// When the running backup finishes, the queue must hand the slot to the
// next machine on its own - the whole point is that waiting devices don't
// have to poll.
func TestReleaseDispatchesNextInLine(t *testing.T) {
	rec := newRecorder()
	m := New(fixedLimit(1), rec.dispatch)

	m.Acquire("pc-a")
	m.Acquire("pc-b")
	m.Acquire("pc-c")

	m.Release("pc-a")
	if got := rec.list(); len(got) != 1 || got[0] != "pc-b" {
		t.Fatalf("appareils relancés = %v, attendu [pc-b] (ordre d'arrivée)", got)
	}

	m.Release("pc-b")
	if got := rec.list(); len(got) != 2 || got[1] != "pc-c" {
		t.Fatalf("appareils relancés = %v, attendu pc-c en second", got)
	}
}

// A queued machine that has been shut down must not consume its turn:
// the slot has to move on to someone who can actually use it.
func TestOfflineDeviceDoesNotHoldUpTheQueue(t *testing.T) {
	rec := newRecorder()
	rec.offline["pc-b"] = true
	m := New(fixedLimit(1), rec.dispatch)

	m.Acquire("pc-a")
	m.Acquire("pc-b") // queued, but will be unreachable when its turn comes
	m.Acquire("pc-c")

	m.Release("pc-a")

	got := rec.list()
	if len(got) != 1 || got[0] != "pc-c" {
		t.Fatalf("appareils relancés = %v, attendu [pc-c] (pc-b hors ligne doit être sauté)", got)
	}
}

// An operator raising the limit from the panel should take effect on the
// next release without restarting the server.
func TestRaisingTheLimitReleasesSeveralAtOnce(t *testing.T) {
	rec := newRecorder()
	limit := 1
	var mu sync.Mutex
	m := New(func() int { mu.Lock(); defer mu.Unlock(); return limit }, rec.dispatch)

	m.Acquire("pc-a")
	m.Acquire("pc-b")
	m.Acquire("pc-c")

	mu.Lock()
	limit = 3
	mu.Unlock()

	m.Release("pc-a")
	if got := rec.list(); len(got) != 2 {
		t.Fatalf("appareils relancés = %v, attendu 2 (limite portée à 3)", got)
	}
}

// A machine that dies mid-backup (power cut) never releases its slot, so
// without a stale sweep the queue would stay shut for everyone.
func TestStaleSlotIsReclaimed(t *testing.T) {
	rec := newRecorder()
	m := New(fixedLimit(1), rec.dispatch)

	m.Acquire("pc-a")
	m.Acquire("pc-b")

	if stale := m.ReleaseStale(time.Hour); len(stale) != 0 {
		t.Fatalf("aucun créneau ne devait être considéré comme bloqué, obtenu %v", stale)
	}

	stale := m.ReleaseStale(0) // everything older than "now" counts as stale
	if len(stale) != 1 || stale[0] != "pc-a" {
		t.Fatalf("créneaux bloqués = %v, attendu [pc-a]", stale)
	}
	if got := rec.list(); len(got) != 1 || got[0] != "pc-b" {
		t.Fatalf("appareils relancés = %v, attendu [pc-b] après libération du créneau bloqué", got)
	}
}

// Re-asking for a slot you already hold must not deadlock you behind
// your own backup.
func TestAcquireIsIdempotentForRunningDevice(t *testing.T) {
	m := New(fixedLimit(1), newRecorder().dispatch)
	m.Acquire("pc-a")
	if granted, _ := m.Acquire("pc-a"); !granted {
		t.Fatal("un appareil qui détient déjà un créneau doit le conserver")
	}
}

func TestConcurrentAcquireReleaseIsRaceFree(t *testing.T) {
	m := New(fixedLimit(2), newRecorder().dispatch)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%7))
			m.Acquire(id)
			m.Position(id)
			m.Status()
			m.Release(id)
		}(i)
	}
	wg.Wait()
}
