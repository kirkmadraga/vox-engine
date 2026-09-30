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

type channelKey struct {
	channel snowflake.ID
	command string
}

// Memory is a non-persistent Backend for tests.
type Memory struct {
	mu       sync.RWMutex
	guilds   map[snowflake.ID]access.Entry
	grants   map[grantKey]access.Entry
	channels map[channelKey]access.Entry
}

var _ access.Backend = (*Memory)(nil)

// NewMemory returns an empty in-memory backend.
func NewMemory() *Memory {
	return &Memory{guilds: map[snowflake.ID]access.Entry{}, grants: map[grantKey]access.Entry{}, channels: map[channelKey]access.Entry{}}
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

func (m *Memory) AllowChannel(_ context.Context, channelID snowflake.ID, command string, e access.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := channelKey{channelID, command}
	if _, ok := m.channels[k]; ok {
		return false, nil
	}
	m.channels[k] = e
	return true, nil
}

func (m *Memory) DenyChannel(_ context.Context, channelID snowflake.ID, command string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := channelKey{channelID, command}
	if _, ok := m.channels[k]; !ok {
		return false, nil
	}
	delete(m.channels, k)
	return true, nil
}

func (m *Memory) ChannelAllowed(_ context.Context, channelID snowflake.ID, command string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.channels[channelKey{channelID, command}]
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
	for k, e := range m.channels {
		s.Channels = append(s.Channels, access.ChannelEntry{ChannelID: k.channel, Command: k.command, Entry: e})
	}
	s.Sort()
	return s, nil
}
