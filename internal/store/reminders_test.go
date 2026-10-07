package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/kirkmadraga/vox-engine/internal/remind"
)

func TestReminders(t *testing.T) {
	path := tempDB(t)
	db := openAt(t, path)
	ctx := context.Background()
	base := time.Date(2026, 10, 7, 6, 0, 0, 0, time.UTC)
	weekly := remind.Rule{Days: [7]bool{false, true}, Hour: 19}
	add := func(user, guild uint64, next time.Duration, rule remind.Rule) int64 {
		t.Helper()
		id, err := db.AddReminder(ctx, remind.Reminder{UserID: snowflakeID(user), UserName: "Alice", GuildID: snowflakeID(guild),
			ChannelID: 5, Text: "stretch", Rule: rule, Location: "Asia/Tokyo", Next: base.Add(next), Created: base})
		if err != nil || id == 0 {
			t.Fatalf("add: %d, %v", id, err)
		}
		return id
	}
	if _, ok, err := db.SoonestReminder(ctx); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	late := add(8, 100, 3*time.Hour, weekly)
	soon := add(8, 100, time.Hour, remind.Rule{})
	other := add(9, 200, 2*time.Hour, remind.Rule{Every: 2 * time.Hour})

	r, ok, err := db.SoonestReminder(ctx)
	if err != nil || !ok || r.ID != soon || r.UserID != 8 || r.GuildID != 100 || r.ChannelID != 5 || r.Text != "stretch" ||
		r.UserName != "Alice" || r.Location != "Asia/Tokyo" || !r.Next.Equal(base.Add(time.Hour)) || !r.Created.Equal(base) || !r.Rule.Once() {
		t.Fatalf("soonest: %+v, %v, %v", r, ok, err)
	}
	mine, err := db.UserReminders(ctx, 8)
	if err != nil || len(mine) != 2 || mine[0].ID != soon || mine[1].ID != late || mine[1].Rule != weekly {
		t.Fatalf("user 8: %+v, %v", mine, err)
	}
	if total, repeating, err := db.ReminderCounts(ctx); total != 3 || repeating != 2 || err != nil {
		t.Errorf("counts: %d %d %v", total, repeating, err)
	}

	if err := db.RescheduleReminder(ctx, soon, base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := db.SoonestReminder(ctx); r.ID != other {
		t.Errorf("after rescheduling, soonest = %d, want %d", r.ID, other)
	}
	if ok, err := db.DeleteReminder(ctx, other); !ok || err != nil {
		t.Errorf("delete: %v %v", ok, err)
	}
	if ok, _ := db.DeleteReminder(ctx, other); ok {
		t.Error("deleting twice reported a change")
	}
	third := add(9, 100, 30*time.Minute, remind.Rule{})
	all, err := db.AllReminders(ctx)
	if err != nil || len(all) != 3 || all[0].ID != third || all[1].ID != late || all[2].ID != soon {
		t.Fatalf("all, soonest first: %+v, %v", all, err)
	}
	for _, id := range []int64{third, late, soon} {
		db.DeleteReminder(ctx, id)
	}
	if all, err := db.AllReminders(ctx); len(all) != 0 || err != nil {
		t.Errorf("left: %+v %v", all, err)
	}
}

// A v1.0.0 database (schema 3) upgrades in place and gains the table.
func TestUpgradeFromSchema3AddsReminders(t *testing.T) {
	path := tempDB(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{}, migrations[:3]...),
		`PRAGMA user_version = 3`,
		`INSERT INTO ask_balances (user_id, day, balance) VALUES (8, '2026-10-07', 4)`) {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db := openAt(t, path)
	ctx := context.Background()
	if _, b, found, err := db.DailyBalance(ctx, 8); !found || b != 4 || err != nil {
		t.Errorf("balance after upgrade: %d %v %v", b, found, err)
	}
	if _, err := db.AddReminder(ctx, remind.Reminder{UserID: 8, GuildID: 1, ChannelID: 2, Text: "x", Next: time.Now(), Created: time.Now()}); err != nil {
		t.Errorf("add after upgrade: %v", err)
	}
}

func snowflakeID(n uint64) snowflake.ID { return snowflake.ID(n) }
