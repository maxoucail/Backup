// Package queue serializes backups across devices.
//
// The server is the only party that knows what every machine is doing, so
// it - not the agents - decides who gets to run. An agent asks for a slot
// when it wants to start; if the configured concurrency limit is already
// reached it is put in a FIFO queue and told to wait, and the server sends
// it a start command as soon as a slot frees up. This keeps one machine
// pushing a large first backup from starving everyone else's NAS
// bandwidth, without agents having to coordinate among themselves.
package queue

import (
	"log"
	"sync"
	"time"
)

// DispatchFunc asks a device to start backing up now. It returns false if
// the device is unreachable (offline), in which case the slot is offered
// to the next device in line instead.
type DispatchFunc func(deviceID string) bool

// MaxConcurrentFunc reads the current limit. It's a function rather than a
// value because an operator can change it from the panel at any time and
// the queue must honour the new value without a restart.
type MaxConcurrentFunc func() int

type waiter struct {
	deviceID string
	since    time.Time
}

type Manager struct {
	mu       sync.Mutex
	running  map[string]time.Time // deviceID -> when its slot was granted
	waiting  []waiter
	maxFn    MaxConcurrentFunc
	dispatch DispatchFunc
}

func New(maxFn MaxConcurrentFunc, dispatch DispatchFunc) *Manager {
	return &Manager{
		running:  make(map[string]time.Time),
		maxFn:    maxFn,
		dispatch: dispatch,
	}
}

func (m *Manager) limit() int {
	n := m.maxFn()
	if n < 1 {
		return 1
	}
	return n
}

// Acquire grants a backup slot to a device. When it returns granted=false
// the device has been queued and position tells it how many devices are
// ahead of it; it must wait for the server to call it back rather than
// starting anyway.
func (m *Manager) Acquire(deviceID string) (granted bool, position int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Already running: re-entrant call (an agent retrying) keeps its slot
	// rather than deadlocking itself behind its own backup.
	if _, ok := m.running[deviceID]; ok {
		return true, 0
	}

	if len(m.running) < m.limit() {
		m.removeWaitingLocked(deviceID)
		m.running[deviceID] = time.Now()
		return true, 0
	}

	for i, w := range m.waiting {
		if w.deviceID == deviceID {
			return false, i + 1
		}
	}
	m.waiting = append(m.waiting, waiter{deviceID: deviceID, since: time.Now()})
	return false, len(m.waiting)
}

// Release frees a device's slot and hands it to the next device waiting.
// Safe to call for a device that holds no slot (a finished backup that was
// never queued, a disconnect after completion).
func (m *Manager) Release(deviceID string) {
	m.mu.Lock()
	delete(m.running, deviceID)
	m.removeWaitingLocked(deviceID)
	next := m.popDispatchableLocked()
	m.mu.Unlock()

	// Dispatch outside the lock: it reaches into the WebSocket hub, and
	// holding two locks across packages is how deadlocks get written.
	m.dispatchAll(next)
}

// Forget drops a device from the queue entirely - used when it goes
// offline while waiting, so its turn isn't wasted on a machine that
// can't act on it.
func (m *Manager) Forget(deviceID string) {
	m.Release(deviceID)
}

// popDispatchableLocked reserves slots for as many waiting devices as the
// current limit allows and returns them, so a raised limit or several
// simultaneous releases can start more than one device.
func (m *Manager) popDispatchableLocked() []string {
	var chosen []string
	for len(m.waiting) > 0 && len(m.running) < m.limit() {
		w := m.waiting[0]
		m.waiting = m.waiting[1:]
		m.running[w.deviceID] = time.Now()
		chosen = append(chosen, w.deviceID)
	}
	return chosen
}

func (m *Manager) dispatchAll(deviceIDs []string) {
	for _, id := range deviceIDs {
		if m.dispatch != nil && m.dispatch(id) {
			log.Printf("file d'attente: c'est au tour de l'appareil %s", id)
			continue
		}
		// Offline: give the slot back and let whoever is next have it.
		log.Printf("file d'attente: appareil %s injoignable, créneau réattribué", id)
		m.mu.Lock()
		delete(m.running, id)
		next := m.popDispatchableLocked()
		m.mu.Unlock()
		m.dispatchAll(next)
	}
}

func (m *Manager) removeWaitingLocked(deviceID string) {
	for i, w := range m.waiting {
		if w.deviceID == deviceID {
			m.waiting = append(m.waiting[:i], m.waiting[i+1:]...)
			return
		}
	}
}

// ReleaseStale frees slots held longer than maxAge. A slot is normally
// released when the backup finishes or the agent disconnects, but a
// machine that vanishes mid-backup without a clean socket close (power
// cut, forced sleep) would otherwise hold the queue shut forever.
func (m *Manager) ReleaseStale(maxAge time.Duration) []string {
	m.mu.Lock()
	cutoff := time.Now().Add(-maxAge)
	var stale []string
	for deviceID, since := range m.running {
		if since.Before(cutoff) {
			stale = append(stale, deviceID)
			delete(m.running, deviceID)
		}
	}
	next := m.popDispatchableLocked()
	m.mu.Unlock()

	m.dispatchAll(next)
	return stale
}

type Status struct {
	Running []string `json:"running"`
	Waiting []string `json:"waiting"`
	Limit   int      `json:"limit"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Limit: m.limit(), Running: make([]string, 0, len(m.running)), Waiting: make([]string, 0, len(m.waiting))}
	for id := range m.running {
		s.Running = append(s.Running, id)
	}
	for _, w := range m.waiting {
		s.Waiting = append(s.Waiting, w.deviceID)
	}
	return s
}

// Position reports how many devices are ahead of this one, 0 if it isn't
// waiting.
func (m *Manager) Position(deviceID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, w := range m.waiting {
		if w.deviceID == deviceID {
			return i + 1
		}
	}
	return 0
}
