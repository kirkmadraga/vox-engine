// Package jsonfile is an access.Backend that keeps the allow-lists in memory and
// writes every change through to a JSON file, atomically.
package jsonfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"bot/internal/access"
)

const version = 1

// entry and file are the on-disk format. Map keys are Discord IDs as decimal strings.
type entry struct {
	AddedBy snowflake.ID `json:"added_by"`
	AddedAt time.Time    `json:"added_at"`
}

type file struct {
	Version int                               `json:"version"`
	Guilds  map[snowflake.ID]entry            `json:"guilds"`
	Grants  map[snowflake.ID]map[string]entry `json:"grants"`
}

func emptyFile() file {
	return file{Version: version, Guilds: map[snowflake.ID]entry{}, Grants: map[snowflake.ID]map[string]entry{}}
}

func (f file) clone() file {
	c := file{Version: f.Version, Guilds: maps.Clone(f.Guilds), Grants: make(map[snowflake.ID]map[string]entry, len(f.Grants))}
	for u, g := range f.Grants {
		c.Grants[u] = maps.Clone(g)
	}
	return c
}

// Store is the JSON file backend.
type Store struct {
	path string
	mu   sync.RWMutex
	st   file
}

var _ access.Backend = (*Store)(nil)

// Open loads path. A missing file starts empty; an unreadable or corrupt file
// is an error, never a silent reset.
func Open(path string) (*Store, error) {
	s := &Store{path: path, st: emptyFile()}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access state: %w", err)
	}
	st := emptyFile()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("access state file %s is corrupt (fix or remove it): %w", path, err)
	}
	if st.Version != version {
		return nil, fmt.Errorf("access state file %s has version %d, want %d", path, st.Version, version)
	}
	if st.Guilds == nil {
		st.Guilds = map[snowflake.ID]entry{}
	}
	if st.Grants == nil {
		st.Grants = map[snowflake.ID]map[string]entry{}
	}
	s.st = st
	return s, nil
}

func (s *Store) AllowGuild(_ context.Context, guildID snowflake.ID, e access.Entry) (bool, error) {
	return s.update(func(f *file) bool {
		if _, ok := f.Guilds[guildID]; ok {
			return false
		}
		f.Guilds[guildID] = entry(e)
		return true
	})
}

func (s *Store) DenyGuild(_ context.Context, guildID snowflake.ID) (bool, error) {
	return s.update(func(f *file) bool {
		if _, ok := f.Guilds[guildID]; !ok {
			return false
		}
		delete(f.Guilds, guildID)
		return true
	})
}

func (s *Store) GuildAllowed(_ context.Context, guildID snowflake.ID) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.st.Guilds[guildID]
	return ok, nil
}

func (s *Store) Grant(_ context.Context, userID snowflake.ID, command string, e access.Entry) (bool, error) {
	return s.update(func(f *file) bool {
		if _, ok := f.Grants[userID][command]; ok {
			return false
		}
		if f.Grants[userID] == nil {
			f.Grants[userID] = map[string]entry{}
		}
		f.Grants[userID][command] = entry(e)
		return true
	})
}

func (s *Store) Revoke(_ context.Context, userID snowflake.ID, command string) (bool, error) {
	return s.update(func(f *file) bool {
		if _, ok := f.Grants[userID][command]; !ok {
			return false
		}
		delete(f.Grants[userID], command)
		if len(f.Grants[userID]) == 0 {
			delete(f.Grants, userID)
		}
		return true
	})
}

func (s *Store) HasGrant(_ context.Context, userID snowflake.ID, command string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.st.Grants[userID][command]
	return ok, nil
}

func (s *Store) Snapshot(context.Context) (access.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var snap access.Snapshot
	for id, e := range s.st.Guilds {
		snap.Guilds = append(snap.Guilds, access.GuildEntry{GuildID: id, Entry: access.Entry(e)})
	}
	for user, cmds := range s.st.Grants {
		for cmd, e := range cmds {
			snap.Grants = append(snap.Grants, access.GrantEntry{UserID: user, Command: cmd, Entry: access.Entry(e)})
		}
	}
	snap.Sort()
	return snap, nil
}

// update applies mutate to a copy, writes the copy to disk, and only then makes
// it current, so a failed write leaves memory unchanged.
func (s *Store) update(mutate func(*file) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st.clone()
	if !mutate(&next) {
		return false, nil
	}
	if err := write(s.path, next); err != nil {
		return false, fmt.Errorf("save access state: %w", err)
	}
	s.st = next
	return true, nil
}

// write writes f to a temp file in the same directory, flushes and closes it,
// then renames it over path, so a crash never leaves a partial file.
func write(path string, f file) (err error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".access-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	// Close before rename: Windows cannot rename an open file.
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
