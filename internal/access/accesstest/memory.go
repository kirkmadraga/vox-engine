// Package accesstest provides an in-memory access.Backend and the contract
// test suite every Backend implementation must pass.
package accesstest

import (
	"context"
	"sync"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
)

type grantKey struct {
	user    snowflake.ID
	command string
}

type openKey struct {
	guild   snowflake.ID
	command string
}

// Memory is a non-persistent Backend for tests.
type Memory struct {
	mu     sync.RWMutex
	guilds map[snowflake.ID]access.Entry
	grants map[grantKey]access.Entry
	open   map[openKey]access.Entry
}

var _ access.Backend = (*Memory)(nil)

// NewMemory returns an empty in-memory backend.
func NewMemory() *Memory {
	return &Memory{guilds: map[snowflake.ID]access.Entry{}, grants: map[grantKey]access.Entry{}, open: map[openKey]access.Entry{}}
}

func (m *Memory) OpenCommand(_ context.Context, guildID snowflake.ID, command string, e access.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := openKey{guildID, command}
	if _, ok := m.open[k]; ok {
		return false, nil
	}
	m.open[k] = e
	return true, nil
}

func (m *Memory) CloseCommand(_ context.Context, guildID snowflake.ID, command string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := openKey{guildID, command}
	if _, ok := m.open[k]; !ok {
		return false, nil
	}
	delete(m.open, k)
	return true, nil
}

func (m *Memory) CommandOpen(_ context.Context, guildID snowflake.ID, command string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.open[openKey{guildID, command}]
	return ok, nil
}

func (m *Memory) AllowGuild(_ context.Context, guildID snowflake.ID, e access.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.guilds[guildID]; ok {
		return false, nil
	}
	m.guilds[guildID] = e
	return true, nil
}

func (m *Memory) DenyGuild(_ context.Context, guildID snowflake.ID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.guilds[guildID]; !ok {
		return false, nil
	}
	delete(m.guilds, guildID)
	return true, nil
}

func (m *Memory) GuildAllowed(_ context.Context, guildID snowflake.ID) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.guilds[guildID]
	return ok, nil
}

func (m *Memory) Grant(_ context.Context, userID snowflake.ID, command string, e access.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := grantKey{userID, command}
	if _, ok := m.grants[k]; ok {
		return false, nil
	}
	m.grants[k] = e
	return true, nil
}

func (m *Memory) Revoke(_ context.Context, userID snowflake.ID, command string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := grantKey{userID, command}
	if _, ok := m.grants[k]; !ok {
		return false, nil
	}
	delete(m.grants, k)
	return true, nil
}

func (m *Memory) HasGrant(_ context.Context, userID snowflake.ID, command string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.grants[grantKey{userID, command}]
	return ok, nil
}

func (m *Memory) Snapshot(context.Context) (access.Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var s access.Snapshot
	for id, e := range m.guilds {
		s.Guilds = append(s.Guilds, access.GuildEntry{GuildID: id, Entry: e})
	}
	for k, e := range m.grants {
		s.Grants = append(s.Grants, access.GrantEntry{UserID: k.user, Command: k.command, Entry: e})
	}
	for k, e := range m.open {
		s.Open = append(s.Open, access.OpenEntry{GuildID: k.guild, Command: k.command, Entry: e})
	}
	s.Sort()
	return s, nil
}
