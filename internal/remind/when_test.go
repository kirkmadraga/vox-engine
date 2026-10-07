package remind

import (
	"errors"
	"testing"
	"time"
)

// A Wednesday, 14:00 in UTC+8.
var (
	zone8 = time.FixedZone("UTC+8", 8*3600)
	now   = time.Date(2026, 10, 7, 14, 0, 0, 0, zone8)
)

func at(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, zone8) }

func TestParseOneOff(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		text string
	}{
		{"in 45m stretch", now.Add(45 * time.Minute), "stretch"},
		{"In 1h30m stretch your legs", now.Add(90 * time.Minute), "stretch your legs"},
		{"in 3d x", now.Add(72 * time.Hour), "x"},
		{"in 2w x", now.Add(14 * 24 * time.Hour), "x"},
		{"at 18:30 call mom", at(2026, 10, 7, 18, 30), "call mom"},
		{"at 9:00 x", at(2026, 10, 8, 9, 0), "x"},   // passed today: tomorrow
		{"at 14:00 x", at(2026, 10, 8, 14, 0), "x"}, // exactly now: tomorrow
		{"at 6:30pm x", at(2026, 10, 7, 18, 30), "x"},
		{"at 6pm x", at(2026, 10, 7, 18, 0), "x"},
		{"at 6 pm x", at(2026, 10, 7, 18, 0), "x"},
		{"at 12am x", at(2026, 10, 8, 0, 0), "x"},
		{"at 12pm x", at(2026, 10, 8, 12, 0), "x"},
		{"tomorrow x", at(2026, 10, 8, 9, 0), "x"},
		{"tomorrow at 7:15am x", at(2026, 10, 8, 7, 15), "x"},
		{"on friday x", at(2026, 10, 9, 9, 0), "x"},
		{"on wed x", at(2026, 10, 14, 9, 0), "x"}, // today is Wednesday: next week's
		{"on fri at 20:00 x", at(2026, 10, 9, 20, 0), "x"},
		{"on 2026-12-24 wrap gifts", at(2026, 12, 24, 9, 0), "wrap gifts"},
		{"on 2026-12-24 at 20:00 x", at(2026, 12, 24, 20, 0), "x"},
		{"on 12-24 x", at(2026, 12, 24, 9, 0), "x"},
		{"on 3-1 x", at(2027, 3, 1, 9, 0), "x"},    // no year: the next one coming
		{"on 10-07 x", at(2027, 10, 7, 9, 0), "x"}, // today 09:00 has passed
		{"in 2h", now.Add(2 * time.Hour), ""},      // no text: the caller refuses
	}
	for _, c := range cases {
		w, text, err := Parse(c.in, now, zone8)
		if err != nil || !w.At.Equal(c.want) || !w.Rule.Once() || text != c.text {
			t.Errorf("%q: at %v rule %+v text %q err %v; want %v %q", c.in, w.At, w.Rule, text, err, c.want, c.text)
		}
	}
}

func TestParseRepeating(t *testing.T) {
	cases := []struct {
		in    string
		first time.Time
		desc  string
	}{
		{"every 2h drink water", now.Add(2 * time.Hour), "every 2h"},
		{"every 1h30m x", now.Add(90 * time.Minute), "every 1h 30m"},
		{"every 3d x", now.Add(72 * time.Hour), "every 3d"},
		{"every day at 08:00 standup", at(2026, 10, 8, 8, 0), "every day at 08:00"},
		{"daily at 8am x", at(2026, 10, 8, 8, 0), "every day at 08:00"},
		{"every day x", at(2026, 10, 8, 9, 0), "every day at 09:00"},
		{"every day at 15:00 x", at(2026, 10, 7, 15, 0), "every day at 15:00"}, // later today
		{"every weekday at 9:00 x", at(2026, 10, 8, 9, 0), "every weekday at 09:00"},
		{"every weekend at 10:00 x", at(2026, 10, 10, 10, 0), "every weekend at 10:00"},
		{"every monday at 19:00 raid", at(2026, 10, 12, 19, 0), "every Monday at 19:00"},
		{"every mon,thu at 19:00 x", at(2026, 10, 8, 19, 0), "every Monday, Thursday at 19:00"},
		{"every sun,sat at 19:00 x", at(2026, 10, 10, 19, 0), "every weekend at 19:00"},
	}
	for _, c := range cases {
		w, _, err := Parse(c.in, now, zone8)
		if err != nil || !w.At.Equal(c.first) || w.Rule.Once() || w.Rule.String() != c.desc {
			t.Errorf("%q: first %v (%q) err %v; want %v (%q)", c.in, w.At, w.Rule.String(), err, c.first, c.desc)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, in := range []string{
		"", "stretch", "in", "in soon x", "in 2 hours x", "in 0m x", "in 2x x", "in h x",
		"at 6 x", // ambiguous hour
		"at 25:00 x", "at 18:5 x", "at 13pm x", "at 0am x", "at x",
		"on someday x", "on 2026-02-30 x", "on 13-01 x", "on 26-12-24 x", "on 03/04 x",
		"every x", "every 30m x", // under the minimum
		"every funday at 9:00 x", "every mon,funday at 9:00 x",
		"in 30s x",                 // no seconds
		"in 400d x",                // over a year
		"on 2025-01-01 x",          // past
		"tonight x", "next week x", // fuzzy words aren't guessed
	} {
		if w, text, err := Parse(in, now, zone8); err == nil {
			t.Errorf("%q: want an error, got %v %+v %q", in, w.At, w.Rule, text)
		}
	}
	if _, _, err := Parse("soon x", now, zone8); !errors.Is(err, ErrUsage) {
		t.Errorf("an unknown start: %v, want ErrUsage", err)
	}
}

func TestRuleNext(t *testing.T) {
	every2h := Rule{Every: 2 * time.Hour}
	// The bot was down for 5 hours: the rhythm is kept, with no burst.
	if got := every2h.Next(now, now.Add(5*time.Hour), zone8); !got.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("every 2h after a gap: %v", got)
	}
	weekdays := Rule{Days: [7]bool{false, true, true, true, true, true, false}, Hour: 9}
	friday := at(2026, 10, 9, 9, 0)
	if got := weekdays.Next(friday, friday, zone8); !got.Equal(at(2026, 10, 12, 9, 0)) {
		t.Errorf("weekdays after Friday: %v, want Monday", got)
	}
	if got := (Rule{}).Next(now, now, zone8); !got.IsZero() {
		t.Errorf("a one-off repeats: %v", got)
	}
}

// Day-based rules keep the wall-clock time across a daylight-saving change.
func TestRuleNextAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	daily := Rule{Days: [7]bool{true, true, true, true, true, true, true}, Hour: 8}
	before := time.Date(2026, 10, 24, 8, 0, 0, 0, berlin) // summer time ends on Oct 25
	got := daily.Next(before, before, berlin)
	if want := time.Date(2026, 10, 25, 8, 0, 0, 0, berlin); !got.Equal(want) || got.Sub(before) != 25*time.Hour {
		t.Errorf("across DST: %v (%v later), want %v", got, got.Sub(before), want)
	}
}

func TestRuleEncodeRoundTrip(t *testing.T) {
	for _, r := range []Rule{
		{},
		{Every: 90 * time.Minute},
		{Days: [7]bool{true, false, false, false, false, false, true}, Hour: 10, Minute: 5},
	} {
		got, err := DecodeRule(r.Encode())
		if err != nil || got != r {
			t.Errorf("%+v -> %q -> %+v, %v", r, r.Encode(), got, err)
		}
	}
	for _, bad := range []string{"every:x", "every:0", "days:1111111:24:00", "days:0000000:09:00", "days:111:09:00", "weekly"} {
		if _, err := DecodeRule(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
