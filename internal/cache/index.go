package cache

import (
	"context"
	"time"
)

// Record is what the index knows about one cached video.
type Record struct {
	ID         string
	Title      string        // empty if unknown
	Duration   time.Duration // 0 if unknown
	Size       int64         // bytes of the .opus file
	AddedAt    time.Time
	LastPlayed time.Time // zero if never played
	PlayCount  int
}

// LastUsed is when the entry last mattered: its last play, else its download.
func (r Record) LastUsed() time.Time {
	if r.LastPlayed.After(r.AddedAt) {
		return r.LastPlayed
	}
	return r.AddedAt
}

// Index records which videos are cached. It is the authority on what belongs
// in the cache directory: video files it doesn't know about are deleted at
// startup. The SQLite store implements it; tests use cachetest.Memory. Every
// implementation must pass cachetest.IndexContract.
type Index interface {
	// Put inserts or replaces a record.
	Put(ctx context.Context, r Record) error
	// Get returns id's record, and whether it exists.
	Get(ctx context.Context, id string) (Record, bool, error)
	// MarkPlayed sets LastPlayed to at and increments PlayCount. Unknown ids are ignored.
	MarkPlayed(ctx context.Context, id string, at time.Time) error
	// Delete removes id's record; deleting an unknown id is not an error.
	Delete(ctx context.Context, id string) error
	// All returns every record, sorted by ID.
	All(ctx context.Context) ([]Record, error)
}
