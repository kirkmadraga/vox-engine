package remind

import (
	"errors"
	"slices"
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
		{"on wed x", at(2026, 10, 14, 9, 0), "x"},                // today is Wednesday, 09:00 passed: next week's
		{"on wednesday at 20:00 x", at(2026, 10, 7, 20, 0), "x"}, // ...but 20:00 is still ahead: today
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
		"daily at 8am x", // not a form: "every day at 8am"
		"on +12-24 x", "on 12-+24 x", "on 2026-012-24 x",
		// Huge numbers used to overflow and wrap into a plausible duration.
		"every 213505d x", "every 30501w x", "in 213505d x", "in 999999w x", "every 999999w999999d x",
	} {
		if w, text, err := Parse(in, now, zone8); err == nil {
			t.Errorf("%q: want an error, got %v %+v %q", in, w.At, w.Rule, text)
		}
	}
	if _, _, err := Parse("soon x", now, zone8); !errors.Is(err, ErrUsage) {
		t.Errorf("an unknown start: %v, want ErrUsage", err)
	}
}

// endOf is the last second of a day in zone8.
func endOf(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, 23, 59, 59, 0, zone8) }

func TestParseEnds(t *testing.T) {
	cases := []struct {
		in    string
		until time.Time
		text  string
	}{
		{"every 2h until 18:00 drink water", at(2026, 10, 7, 18, 0), "drink water"},
		{"every 2h until 13:00 x", at(2026, 10, 8, 13, 0), "x"}, // passed today: tomorrow's
		{"every 2h until 14:00 x", at(2026, 10, 8, 14, 0), "x"}, // exactly now: tomorrow's
		{"every 2h until 6pm x", at(2026, 10, 7, 18, 0), "x"},
		{"every 2h until 6 pm x", at(2026, 10, 7, 18, 0), "x"},
		{"every day at 08:00 until friday standup", endOf(2026, 10, 9), "standup"}, // the whole day
		{"every day at 08:00 until friday at 17:00 x", at(2026, 10, 9, 17, 0), "x"},
		{"every 2h UNTIL Friday x", endOf(2026, 10, 9), "x"},
		{"every 2h until wednesday x", endOf(2026, 10, 7), "x"},         // today is Wednesday: through today
		{"every 2h until wed at 20:00 x", at(2026, 10, 7, 20, 0), "x"},  // still ahead: today
		{"every 1h until wed at 13:00 x", at(2026, 10, 14, 13, 0), "x"}, // passed: next week's
		{"every 2h until tomorrow x", endOf(2026, 10, 8), "x"},          // the whole day
		{"every 2h until tomorrow at 12:00 x", at(2026, 10, 8, 12, 0), "x"},
		{"every day until 2026-12-24 timesheet", endOf(2026, 12, 24), "timesheet"},
		{"every day until 12-24 x", endOf(2026, 12, 24), "x"},
		{"every 2h until 10-07 x", endOf(2026, 10, 7), "x"}, // today, still going
		{"every 2h until 10-06 x", endOf(2027, 10, 6), "x"}, // no year: the next one
		{"every 2h until 2026-12-24 at 17:00 x", at(2026, 12, 24, 17, 0), "x"},
		{"every 2h for 3h x", now.Add(3 * time.Hour), "x"},
		{"every 2h for 2h x", now.Add(2 * time.Hour), "x"}, // ends exactly at the first: still fires once
		{"every day at 08:00 for 1w vitamins", now.Add(7 * 24 * time.Hour), "vitamins"},
		{"every 2h FOR 1d x", now.Add(24 * time.Hour), "x"},
		// Text that merely contains the words.
		{"every 2h until 18:00 for the kids", at(2026, 10, 7, 18, 0), "for the kids"},
		{"every 2h call until 5", time.Time{}, "call until 5"},
		{"every day at 08:00 for the standup", time.Time{}, "for the standup"},
		{"every 2h drink water", time.Time{}, "drink water"},
	}
	for _, c := range cases {
		w, text, err := Parse(c.in, now, zone8)
		if err != nil || !w.Until.Equal(c.until) || text != c.text || w.Rule.Once() {
			t.Errorf("%q: until %v text %q err %v; want %v %q", c.in, w.Until, text, err, c.until, c.text)
		}
	}
}

// After a one-off, "until" and "for" are its text unless they read as an end,
// which only a repeat can have.
func TestParseEndsAfterOneOffs(t *testing.T) {
	for in, text := range map[string]string{
		"in 2h wait until mom calls": "wait until mom calls",
		"in 2h until mom calls":      "until mom calls",
		"in 1h for the meeting":      "for the meeting",
		"at 18:00 until later":       "until later",
	} {
		if w, got, err := Parse(in, now, zone8); err != nil || got != text || !w.Until.IsZero() {
			t.Errorf("%q: text %q until %v err %v; want text %q", in, got, w.Until, err, text)
		}
	}
	for _, in := range []string{"in 2h until 18:00 x", "in 2h for 3h x", "at 18:00 until 19:00 x", "tomorrow until friday x", "on friday for 1d x"} {
		if _, _, err := Parse(in, now, zone8); !errors.Is(err, errOnlyRepeats) {
			t.Errorf("%q: %v, want errOnlyRepeats", in, err)
		}
	}
}

func TestParseEndRefusals(t *testing.T) {
	for in, want := range map[string]string{
		"every 2h until 6 x":                  errUntil.Error(), // ambiguous, like "at 6"
		"every 2h until fridy x":              errUntil.Error(),
		"every 2h until mom calls":            errUntil.Error(),
		"every 2h until":                      errUntil.Error(),
		"every 2h until friday at x":          errUntil.Error(),
		"every 2h until 2026-02-30 x":         "there's no such date",
		"every 2h until 2026-01-01 x":         "that end has already passed",
		"every 2h until 2027-12-01 x":         "that end is more than a year away",
		"every 2h for 400d x":                 "that end is more than a year away",
		"every 2h for 1h x":                   "that ends before the first reminder would come",
		"every day at 08:00 until 07:00 x":    "that ends before the first reminder would come", // tomorrow 07:00, first at 08:00
		"every 2h until 18:00 for 2h x":       errBothEnds.Error(),
		"every 2h for 3h until friday x":      errBothEnds.Error(),
		"every 2h until 18:00 until friday x": errBothEnds.Error(),
	} {
		if _, _, err := Parse(in, now, zone8); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

// Ends are whole seconds like the first time, so "for" ending exactly at the
// first reminder still allows it.
func TestParseEndRoundsUp(t *testing.T) {
	almost := time.Date(2026, 10, 7, 18, 29, 30, 500, zone8)
	w, _, err := Parse("every 2h for 2h x", almost, zone8)
	if err != nil || !w.Until.Equal(w.At) || w.Until.Nanosecond() != 0 {
		t.Errorf("for 2h from a fraction of a second: at %v until %v err %v", w.At, w.Until, err)
	}
}

func TestWhenTimes(t *testing.T) {
	parse := func(in string) When {
		w, _, err := Parse(in, now, zone8)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		return w
	}
	cases := []struct {
		in   string
		want []time.Time
		ok   bool
	}{
		{"every 2h for 3h x", []time.Time{at(2026, 10, 7, 16, 0)}, true},
		{"every 2h until 18:00 x", []time.Time{at(2026, 10, 7, 16, 0), at(2026, 10, 7, 18, 0)}, true},
		{"every day at 08:00 until friday x", []time.Time{at(2026, 10, 8, 8, 0), at(2026, 10, 9, 8, 0)}, true},
		{"every 2h until 2026-10-08 at 00:00 x", []time.Time{at(2026, 10, 7, 16, 0), at(2026, 10, 7, 18, 0), at(2026, 10, 7, 20, 0),
			at(2026, 10, 7, 22, 0), at(2026, 10, 8, 0, 0)}, true}, // exactly 5
		{"every 1h until 2026-12-24 x", []time.Time{now.Add(1 * time.Hour), now.Add(2 * time.Hour), now.Add(3 * time.Hour),
			now.Add(4 * time.Hour), now.Add(5 * time.Hour)}, false}, // more than 5
		{"in 2h x", []time.Time{now.Add(2 * time.Hour)}, true},
		{"every 2h x", []time.Time{now.Add(2 * time.Hour)}, false}, // no end
	}
	for _, c := range cases {
		got, ok := parse(c.in).Times(zone8, 5)
		if ok != c.ok || !slices.EqualFunc(got, c.want, time.Time.Equal) {
			t.Errorf("%q: %v %v; want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// "at" less than a minute ahead is still today, and times are whole seconds,
// rounded up so a reminder never fires early.
func TestParseSecondsAndTheNearFuture(t *testing.T) {
	almost := time.Date(2026, 10, 7, 18, 29, 30, 500, zone8) // 30.5 s before 18:30
	if w, _, err := Parse("at 18:30 x", almost, zone8); err != nil || !w.At.Equal(at(2026, 10, 7, 18, 30)) {
		t.Errorf("at 18:30 from 18:29:30: %v, %v", w.At, err)
	}
	if w, _, err := Parse("in 45m x", almost, zone8); err != nil || !w.At.Equal(time.Date(2026, 10, 7, 19, 14, 31, 0, zone8)) {
		t.Errorf("in 45m from 18:29:30.0000005: %v, %v", w.At, err)
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
