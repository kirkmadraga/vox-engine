// Package cachetest provides an in-memory cache.Index and the contract test
// suite every Index implementation must pass.
package cachetest

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/cache"
)

// Memory is a non-persistent Index for tests.
type Memory struct {
	mu   sync.Mutex
	recs map[string]cache.Record
}

var _ cache.Index = (*Memory)(nil)

// NewMemory returns an empty in-memory index.
func NewMemory() *Memory { return &Memory{recs: map[string]cache.Record{}} }

func (m *Memory) Put(_ context.Context, r cache.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.ID] = r
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (cache.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	return r, ok, nil
}

func (m *Memory) MarkPlayed(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[id]; ok {
		r.LastPlayed = at
		r.PlayCount++
		m.recs[id] = r
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, id)
	return nil
}

func (m *Memory) All(context.Context) ([]cache.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []cache.Record
	for _, id := range slices.Sorted(maps.Keys(m.recs)) {
		out = append(out, m.recs[id])
	}
	return out, nil
}
