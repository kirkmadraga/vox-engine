package accesstest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/access"
)

// Factory opens a fresh, empty backend for one test. reopen returns a new
// instance over the same storage (to prove persistence); a non-persistent
// backend may return the same instance.
type Factory func(t *testing.T) (b access.Backend, reopen func(t *testing.T) access.Backend)

// Timestamps use whole seconds in UTC so any storage (JSON, SQL) round-trips them exactly.
var (
	t1 = time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 9, 30, 6, 30, 15, 0, time.UTC)
)

// Large real-world IDs catch float64 precision loss in storage.
const (
	owner  snowflake.ID = 1100000000000000001
	userA  snowflake.ID = 1100000000000000002
	userB  snowflake.ID = 300000000000000001
	guildA snowflake.ID = 1100000000000000003
	guildB snowflake.ID = 200000000000000002
)

// BackendContract runs the behaviour every access.Backend must have. Adding a
// new storage (e.g. a database) means passing this suite unchanged.
func BackendContract(t *testing.T, open Factory) {
	ctx := context.Background()

	t.Run("starts empty", func(t *testing.T) {
		b, _ := open(t)
		snap := snap(t, b)
		if len(snap.Guilds) != 0 || len(snap.Grants) != 0 {
			t.Errorf("snapshot = %+v, want empty", snap)
		}
		if guildOK(t, b, guildA) || grantOK(t, b, userA, "play") {
			t.Error("empty backend reports entries")
		}
	})

	t.Run("guild allow and deny", func(t *testing.T) {
		b, _ := open(t)
		expect(t, "allow", true)(b.AllowGuild(ctx, guildA, access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "allow again", false)(b.AllowGuild(ctx, guildA, access.Entry{AddedBy: userA, AddedAt: t2}))
		if !guildOK(t, b, guildA) || guildOK(t, b, guildB) {
			t.Error("GuildAllowed mismatch after allow")
		}
		snap := snap(t, b)
		if len(snap.Guilds) != 1 || snap.Guilds[0].AddedBy != owner || !snap.Guilds[0].AddedAt.Equal(t1) {
			t.Errorf("second allow must not overwrite the original entry: %+v", snap.Guilds)
		}
		expect(t, "deny", true)(b.DenyGuild(ctx, guildA))
		expect(t, "deny again", false)(b.DenyGuild(ctx, guildA))
		if guildOK(t, b, guildA) {
			t.Error("guild still allowed after deny")
		}
	})

	t.Run("grant and revoke", func(t *testing.T) {
		b, _ := open(t)
		expect(t, "grant", true)(b.Grant(ctx, userA, "play", access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "grant again", false)(b.Grant(ctx, userA, "play", access.Entry{AddedBy: owner, AddedAt: t2}))
		expect(t, "grant other", true)(b.Grant(ctx, userA, "image", access.Entry{AddedBy: owner, AddedAt: t1}))
		if !grantOK(t, b, userA, "play") || grantOK(t, b, userB, "play") {
			t.Error("HasGrant mismatch")
		}
		expect(t, "revoke", true)(b.Revoke(ctx, userA, "play"))
		expect(t, "revoke again", false)(b.Revoke(ctx, userA, "play"))
		expect(t, "revoke missing user", false)(b.Revoke(ctx, userB, "play"))
		if grantOK(t, b, userA, "play") || !grantOK(t, b, userA, "image") {
			t.Error("revoke removed the wrong grant")
		}
	})

	t.Run("deny guild keeps grants", func(t *testing.T) {
		b, _ := open(t)
		expect(t, "allow", true)(b.AllowGuild(ctx, guildA, access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "grant", true)(b.Grant(ctx, userA, "play", access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "deny", true)(b.DenyGuild(ctx, guildA))
		if !grantOK(t, b, userA, "play") {
			t.Error("denying a guild must not remove user grants")
		}
	})

	t.Run("snapshot is sorted and complete", func(t *testing.T) {
		b, _ := open(t)
		expect(t, "allow A", true)(b.AllowGuild(ctx, guildA, access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "allow B", true)(b.AllowGuild(ctx, guildB, access.Entry{AddedBy: userA, AddedAt: t2}))
		expect(t, "grant A play", true)(b.Grant(ctx, userA, "play", access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "grant A image", true)(b.Grant(ctx, userA, "image", access.Entry{AddedBy: owner, AddedAt: t2}))
		expect(t, "grant B play", true)(b.Grant(ctx, userB, "play", access.Entry{AddedBy: owner, AddedAt: t1}))

		got := snap(t, b)
		want := access.Snapshot{
			Guilds: []access.GuildEntry{
				{GuildID: guildB, Entry: access.Entry{AddedBy: userA, AddedAt: t2}},
				{GuildID: guildA, Entry: access.Entry{AddedBy: owner, AddedAt: t1}},
			},
			Grants: []access.GrantEntry{
				{UserID: userB, Command: "play", Entry: access.Entry{AddedBy: owner, AddedAt: t1}},
				{UserID: userA, Command: "image", Entry: access.Entry{AddedBy: owner, AddedAt: t2}},
				{UserID: userA, Command: "play", Entry: access.Entry{AddedBy: owner, AddedAt: t1}},
			},
		}
		if !snapshotsEqual(got, want) {
			t.Errorf("snapshot:\n got  %+v\n want %+v", got, want)
		}
	})

	t.Run("persists across reopen", func(t *testing.T) {
		b, reopen := open(t)
		expect(t, "allow", true)(b.AllowGuild(ctx, guildA, access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "allow B", true)(b.AllowGuild(ctx, guildB, access.Entry{AddedBy: owner, AddedAt: t1}))
		expect(t, "grant", true)(b.Grant(ctx, userA, "play", access.Entry{AddedBy: owner, AddedAt: t2}))
		expect(t, "grant B", true)(b.Grant(ctx, userB, "play", access.Entry{AddedBy: owner, AddedAt: t2}))
		expect(t, "deny B", true)(b.DenyGuild(ctx, guildB))
		expect(t, "revoke B", true)(b.Revoke(ctx, userB, "play"))
		before := snap(t, b)

		after := snap(t, reopen(t))
		if !snapshotsEqual(before, after) {
			t.Errorf("state changed across reopen:\n before %+v\n after  %+v", before, after)
		}
	})

	t.Run("concurrent use", func(t *testing.T) {
		b, _ := open(t)
		const n = 20
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				user := snowflake.ID(1000 + i)
				if _, err := b.Grant(ctx, user, "play", access.Entry{AddedBy: owner, AddedAt: t1}); err != nil {
					t.Errorf("grant %d: %v", i, err)
				}
				_, _ = b.HasGrant(ctx, user, "play")
				_, _ = b.Snapshot(ctx)
			}()
		}
		wg.Wait()
		if got := len(snap(t, b).Grants); got != n {
			t.Errorf("got %d grants, want %d", got, n)
		}
	})
}

func snapshotsEqual(a, b access.Snapshot) bool {
	if len(a.Guilds) != len(b.Guilds) || len(a.Grants) != len(b.Grants) {
		return false
	}
	for i := range a.Guilds {
		x, y := a.Guilds[i], b.Guilds[i]
		if x.GuildID != y.GuildID || x.AddedBy != y.AddedBy || !x.AddedAt.Equal(y.AddedAt) {
			return false
		}
	}
	for i := range a.Grants {
		x, y := a.Grants[i], b.Grants[i]
		if x.UserID != y.UserID || x.Command != y.Command || x.AddedBy != y.AddedBy || !x.AddedAt.Equal(y.AddedAt) {
			return false
		}
	}
	return true
}

// expect returns a helper asserting a mutation's changed flag and no error.
func expect(t *testing.T, what string, wantChanged bool) func(bool, error) {
	t.Helper()
	return func(changed bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if changed != wantChanged {
			t.Errorf("%s: changed = %v, want %v", what, changed, wantChanged)
		}
	}
}

func snap(t *testing.T, b access.Backend) access.Snapshot {
	t.Helper()
	s, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return s
}

func guildOK(t *testing.T, b access.Backend, guildID snowflake.ID) bool {
	t.Helper()
	ok, err := b.GuildAllowed(context.Background(), guildID)
	if err != nil {
		t.Fatalf("GuildAllowed: %v", err)
	}
	return ok
}

func grantOK(t *testing.T, b access.Backend, userID snowflake.ID, command string) bool {
	t.Helper()
	ok, err := b.HasGrant(context.Background(), userID, command)
	if err != nil {
		t.Fatalf("HasGrant: %v", err)
	}
	return ok
}
