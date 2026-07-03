// Package storage is the persistence layer: the Perl getData/saveData
// semantics (one value per namespace+name, namespace = module by
// convention) against Postgres in production and an in-memory backend in
// tests. Unlike the Perl Storable backend, writes land immediately:
// kill -9 loses nothing committed.
package storage

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"
)

// Store is what modules see. Values are marshaled as JSON, so anything
// json.Marshal accepts round-trips. Implementations are safe for
// concurrent use.
type Store interface {
	// Get unmarshals the value at (ns, name) into dst and reports
	// whether it existed.
	Get(ns, name string, dst any) (bool, error)
	// Put upserts the value at (ns, name), immediately and durably.
	Put(ns, name string, v any) error
	// Delete removes (ns, name). Deleting a missing name is not an error.
	Delete(ns, name string) error
	// Names returns the sorted names present in ns.
	Names(ns string) ([]string, error)
	// Close releases backend resources.
	Close() error
}

// Memory is the in-memory Store for tests. It stores marshaled JSON so
// round-trip behavior matches the Postgres backend exactly.
type Memory struct {
	mu   sync.RWMutex
	data map[string]map[string][]byte // ns -> name -> json
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{data: make(map[string]map[string][]byte)}
}

func (m *Memory) Get(ns, name string, dst any) (bool, error) {
	m.mu.RLock()
	raw, ok := m.data[ns][name]
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dst)
}

func (m *Memory) Put(ns, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[ns] == nil {
		m.data[ns] = make(map[string][]byte)
	}
	m.data[ns][name] = raw
	return nil
}

func (m *Memory) Delete(ns, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data[ns], name)
	return nil
}

func (m *Memory) Names(ns string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := slices.Collect(maps.Keys(m.data[ns]))
	slices.Sort(names)
	return names, nil
}

func (m *Memory) Close() error { return nil }
