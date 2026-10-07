package remind

import (
	"context"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Reminder is one saved reminder. Text is the user's own words; it's stored
// to be sent back to them, and never logged.
type Reminder struct {
	ID        int64
	UserID    snowflake.ID
	UserName  string // display name when it was set, for the model's wording
	GuildID   snowflake.ID
	ChannelID snowflake.ID // where it was set, and where it fires
	Text      string
	Rule      Rule
	Location  string    // the timezone it was set in (day-based rules keep it)
	Next      time.Time // when it's due next
	Created   time.Time
}

// Store keeps reminders. store.DB implements it.
type Store interface {
	AddReminder(ctx context.Context, r Reminder) (id int64, err error)
	// UserReminders returns user's reminders, soonest first.
	UserReminders(ctx context.Context, user snowflake.ID) ([]Reminder, error)
	// SoonestReminder returns the reminder due first, if any.
	SoonestReminder(ctx context.Context) (r Reminder, ok bool, err error)
	RescheduleReminder(ctx context.Context, id int64, next time.Time) error
	DeleteReminder(ctx context.Context, id int64) (deleted bool, err error)
	DeleteUserReminders(ctx context.Context, user snowflake.ID) (n int, err error)
	// DeleteGuildReminders deletes the reminders set in guild, except keep's
	// (owners, who keep their access in a denied server).
	DeleteGuildReminders(ctx context.Context, guild snowflake.ID, keep []snowflake.ID) (n int, err error)
	// ReminderCounts counts all reminders, and how many repeat.
	ReminderCounts(ctx context.Context) (total, repeating int, err error)
}

// location loads r's timezone, falling back to UTC.
func (r Reminder) location() *time.Location {
	if loc, err := time.LoadLocation(r.Location); err == nil && r.Location != "" {
		return loc
	}
	return time.UTC
}
