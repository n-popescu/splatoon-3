// Package store is the persistence layer: a small, dependency-free JSON store.
//
// The rest of the Nextendo stack keeps its state as JSON files under a data
// directory (nextendo-account does exactly this), and there is no database in
// the deployment. This package follows that convention so a Splatoon 3 server
// is a single binary plus a directory you can back up with cp.
//
// Writes are debounced: a hot path (a cloud save, a lobby message) mutates
// memory and marks the file dirty, and a background flusher persists it. That
// keeps disk I/O off the request path while still surviving a restart.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONMap is a persisted map[string]T, safe for concurrent use.
type JSONMap[T any] struct {
	path  string
	mu    sync.RWMutex
	items map[string]T
	dirty bool
}

// OpenJSONMap loads (or creates) a JSON map at path.
func OpenJSONMap[T any](path string) (*JSONMap[T], error) {
	m := &JSONMap[T]{path: path, items: map[string]T{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &m.items); err != nil {
			// A corrupt file must not take the server down and must not be
			// silently overwritten either: move it aside so an operator can
			// look at it, and start clean.
			backup := path + ".corrupt." + time.Now().UTC().Format("20060102T150405")
			_ = os.Rename(path, backup)
			m.items = map[string]T{}
			return m, fmt.Errorf("store: %s was unreadable (%v); moved to %s and started empty", path, err, backup)
		}
	}
	return m, nil
}

// Get returns the value for key.
func (m *JSONMap[T]) Get(key string) (T, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.items[key]
	return v, ok
}

// Put stores a value and marks the map dirty.
func (m *JSONMap[T]) Put(key string, v T) {
	m.mu.Lock()
	m.items[key] = v
	m.dirty = true
	m.mu.Unlock()
}

// Update applies fn to the value for key (the zero value when absent) and stores
// the result. fn runs under the lock, so it must not block.
func (m *JSONMap[T]) Update(key string, fn func(cur T, found bool) T) T {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, found := m.items[key]
	next := fn(cur, found)
	m.items[key] = next
	m.dirty = true
	return next
}

// Delete removes a key.
func (m *JSONMap[T]) Delete(key string) {
	m.mu.Lock()
	delete(m.items, key)
	m.dirty = true
	m.mu.Unlock()
}

// Keys returns every key.
func (m *JSONMap[T]) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.items))
	for k := range m.items {
		out = append(out, k)
	}
	return out
}

// Range calls fn for every entry. fn must not call back into the map.
func (m *JSONMap[T]) Range(fn func(key string, v T) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, v := range m.items {
		if !fn(k, v) {
			return
		}
	}
}

// Len returns the number of entries.
func (m *JSONMap[T]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

// Flush persists the map if it changed since the last flush. Writes to a temp
// file and renames, so a crash mid-write cannot truncate the store.
func (m *JSONMap[T]) Flush() error {
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	b, err := json.MarshalIndent(m.items, "", "  ")
	m.dirty = false
	m.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// StartFlusher persists the map every interval until stop is closed. Returns
// immediately; call it once per store.
func (m *JSONMap[T]) StartFlusher(interval time.Duration, stop <-chan struct{}, onErr func(error)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := m.Flush(); err != nil && onErr != nil {
					onErr(err)
				}
			case <-stop:
				if err := m.Flush(); err != nil && onErr != nil {
					onErr(err)
				}
				return
			}
		}
	}()
}
