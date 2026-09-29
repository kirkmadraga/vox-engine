package cachetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kirkmadraga/vox-engine/internal/cache"
)

// Factory opens a fresh, empty index for one test. reopen returns a new
// instance over the same storage (to prove persistence); a non-persistent
// index may return the same instance.
type Factory func(t *testing.T) (idx cache.Index, reopen func(t *testing.T) cache.Index)

// Whole-second UTC times, so any storage round-trips them exactly.
var (
	t1 = time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 9, 29, 6, 30, 15, 0, time.UTC)
)

// IndexContract runs the behaviour every cache.Index must have.
func IndexContract(t *testing.T, open Factory) {
	ctx := context.Background()
	rec := cache.Record{ID: "jNQXAC9IVRw", Title: "Me at the zoo 🐘 \"quotes\"", Duration: 19500 * time.Millisecond, Size: 247109, AddedAt: t1}

	t.Run("starts empty", func(t *testing.T) {
		idx, _ := open(t)
		if all := all(t, idx); len(all) != 0 {
			t.Errorf("All = %+v", all)
		}
		if _, ok, err := idx.Get(ctx, rec.ID); ok || err != nil {
			t.Errorf("Get on empty: ok=%v err=%v", ok, err)
		}
	})

	t.Run("put get round trip", func(t *testing.T) {
		idx, _ := open(t)
		mustOK(t, idx.Put(ctx, rec))
		got, ok, err := idx.Get(ctx, rec.ID)
		if err != nil || !ok || !equal(got, rec) {
			t.Errorf("Get = %+v, %v, %v; want %+v", got, ok, err, rec)
		}
	})

	t.Run("put replaces", func(t *testing.T) {
		idx, _ := open(t)
		mustOK(t, idx.Put(ctx, rec))
		newer := rec
		newer.Title, newer.Size, newer.AddedAt = "New title", 1, t2
		mustOK(t, idx.Put(ctx, newer))
		got, _, _ := idx.Get(ctx, rec.ID)
		if !equal(got, newer) || len(all(t, idx)) != 1 {
			t.Errorf("after replace: %+v", got)
		}
	})

	t.Run("mark played", func(t *testing.T) {
		idx, _ := open(t)
		mustOK(t, idx.Put(ctx, rec))
		mustOK(t, idx.MarkPlayed(ctx, rec.ID, t2))
		mustOK(t, idx.MarkPlayed(ctx, rec.ID, t2.Add(time.Hour)))
		got, _, _ := idx.Get(ctx, rec.ID)
		if got.PlayCount != 2 || !got.LastPlayed.Equal(t2.Add(time.Hour)) {
			t.Errorf("after two plays: %+v", got)
		}
		if !got.LastUsed().Equal(t2.Add(time.Hour)) {
			t.Errorf("LastUsed = %v", got.LastUsed())
		}
		mustOK(t, idx.MarkPlayed(ctx, "unknown0000", t2)) // ignored, not an error
		if len(all(t, idx)) != 1 {
			t.Error("MarkPlayed must not create records")
		}
	})

	t.Run("delete", func(t *testing.T) {
		idx, _ := open(t)
		mustOK(t, idx.Put(ctx, rec))
		mustOK(t, idx.Delete(ctx, rec.ID))
		mustOK(t, idx.Delete(ctx, rec.ID)) // idempotent
		if _, ok, _ := idx.Get(ctx, rec.ID); ok {
			t.Error("record still present after Delete")
		}
	})

	t.Run("all is sorted and complete", func(t *testing.T) {
		idx, _ := open(t)
		for _, id := range []string{"ccccccccccc", "aaaaaaaaaaa", "bbbbbbbbbbb"} {
			mustOK(t, idx.Put(ctx, cache.Record{ID: id, Size: 1, AddedAt: t1}))
		}
		all := all(t, idx)
		if len(all) != 3 || all[0].ID != "aaaaaaaaaaa" || all[2].ID != "ccccccccccc" {
			t.Errorf("All = %+v", all)
		}
	})

	t.Run("persists across reopen", func(t *testing.T) {
		idx, reopen := open(t)
		mustOK(t, idx.Put(ctx, rec))
		mustOK(t, idx.MarkPlayed(ctx, rec.ID, t2))
		want, _, _ := idx.Get(ctx, rec.ID)
		got, ok, err := reopen(t).Get(ctx, rec.ID)
		if err != nil || !ok || !equal(got, want) {
			t.Errorf("after reopen: %+v, %v, %v; want %+v", got, ok, err, want)
		}
	})

	t.Run("concurrent use", func(t *testing.T) {
		idx, _ := open(t)
		const n = 20
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("id%09d", i)
				if err := idx.Put(ctx, cache.Record{ID: id, Size: int64(i), AddedAt: t1}); err != nil {
					t.Errorf("put %d: %v", i, err)
				}
				_ = idx.MarkPlayed(ctx, id, t2)
				_, _ = idx.All(ctx)
			}()
		}
		wg.Wait()
		if got := len(all(t, idx)); got != n {
			t.Errorf("got %d records, want %d", got, n)
		}
	})
}

func equal(a, b cache.Record) bool {
	return a.ID == b.ID && a.Title == b.Title && a.Duration == b.Duration && a.Size == b.Size &&
		a.AddedAt.Equal(b.AddedAt) && a.LastPlayed.Equal(b.LastPlayed) && a.PlayCount == b.PlayCount
}

func all(t *testing.T, idx cache.Index) []cache.Record {
	t.Helper()
	recs, err := idx.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return recs
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
