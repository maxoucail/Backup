package filestore

import "sync"

// Holder lets the storage root be changed at runtime (from the settings
// panel) without restarting the server. It does not move existing backups
// to the new location - the panel warns the operator about that before
// applying the change.
type Holder struct {
	mu    sync.RWMutex
	store *Store
}

func NewHolder(initial *Store) *Holder {
	return &Holder{store: initial}
}

func (h *Holder) Get() *Store {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.store
}

func (h *Holder) Set(s *Store) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store = s
}
