// Package remind parses when a reminder is due ("in 2h", "every monday at
// 19:00") and fires saved reminders on time. The grammar is strict on
// purpose: setting a reminder never depends on a language model, and an
// ambiguous time ("at 6", "03/04") is refused rather than guessed.
package remind

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Limits on what can be scheduled (the bot operator's choices).
const (
	MinInterval = time.Hour            // repeats: protects the channel and the allowance
	MaxAhead    = 366 * 24 * time.Hour // one-off reminders: about a year
	MinAhead    = time.Minute
	defaultHour = 9 // "tomorrow", "on friday", "every day": 09:00 unless a time is given
)

// ErrUsage means the text isn't a when this grammar understands.
var ErrUsage = errors.New("not a time I understand")

// Rule is how a reminder repeats. The zero Rule is a one-off.
type Rule struct {
	Every        time.Duration // fixed interval ("every 2h"); 0 if day-based
	Days         [7]bool       // day-based: which weekdays (index time.Sunday..)
	Hour, Minute int           // day-based: the time of day
}

// Once reports whether the rule doesn't repeat.
func (r Rule) Once() bool { return r.Every == 0 && r.Days == [7]bool{} }

// Next returns the first time after after that r is due again, in loc.
// prev is the last time it was due (for fixed intervals, which keep their
// rhythm). A one-off rule never repeats: it returns the zero time.
func (r Rule) Next(prev, after time.Time, loc *time.Location) time.Time {
	switch {
	case r.Every > 0:
		n := after.Sub(prev)/r.Every + 1
		return prev.Add(n * r.Every)
	case r.Once():
		return time.Time{}
	}
	t := after.In(loc)
	for i := 0; i <= 7; i++ {
		d := time.Date(t.Year(), t.Month(), t.Day()+i, r.Hour, r.Minute, 0, 0, loc)
		if d.After(after) && r.Days[d.Weekday()] {
			return d
		}
	}
	return time.Time{} // unreachable: some day is set
}

// String describes the rule for people, e.g. "every day at 08:00".
func (r Rule) String() string {
	if r.Every > 0 {
		return "every " + formatDuration(r.Every)
	}
	if r.Once() {
		return ""
	}
	at := fmt.Sprintf(" at %02d:%02d", r.Hour, r.Minute)
	switch r.Days {
	case [7]bool{true, true, true, true, true, true, true}:
		return "every day" + at
	case [7]bool{false, true, true, true, true, true, false}:
		return "every weekday" + at
	case [7]bool{true, false, false, false, false, false, true}:
		return "every weekend" + at
	}
	var names []string
	for d := time.Monday; ; d = (d + 1) % 7 { // Monday first, as people list them
		if r.Days[d] {
			names = append(names, d.String())
		}
		if d == time.Sunday {
			break
		}
	}
	return "every " + strings.Join(names, ", ") + at
}

// Encode stores the rule as text: "" (once), "every:<seconds>" or
// "days:<0/1 for Sunday..Saturday>:<HH>:<MM>".
func (r Rule) Encode() string {
	if r.Every > 0 {
		return "every:" + strconv.FormatInt(int64(r.Every/time.Second), 10)
	}
	if r.Once() {
		return ""
	}
	var b strings.Builder
	for _, on := range r.Days {
		if on {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return fmt.Sprintf("days:%s:%02d:%02d", b.String(), r.Hour, r.Minute)
}

// DecodeRule reads Encode's text.
func DecodeRule(s string) (Rule, error) {
	if s == "" {
		return Rule{}, nil
	}
	if secs, ok := strings.CutPrefix(s, "every:"); ok {
		n, err := strconv.ParseInt(secs, 10, 64)
		if err != nil || n <= 0 {
			return Rule{}, fmt.Errorf("bad rule %q", s)
		}
		return Rule{Every: time.Duration(n) * time.Second}, nil
	}
	parts := strings.Split(strings.TrimPrefix(s, "days:"), ":")
	if !strings.HasPrefix(s, "days:") || len(parts) != 3 || len(parts[0]) != 7 {
		return Rule{}, fmt.Errorf("bad rule %q", s)
	}
	var r Rule
	for i, c := range parts[0] {
		r.Days[i] = c == '1'
	}
	h, err1 := strconv.Atoi(parts[1])
	m, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || r.Once() {
		return Rule{}, fmt.Errorf("bad rule %q", s)
	}
	r.Hour, r.Minute = h, m
	return r, nil
}

// When is a parsed time: the first time it's due, and how it repeats.
type When struct {
	At   time.Time
	Rule Rule
}

// Parse reads a when from the start of text and returns it with the rest of
// the text (the reminder itself). now and loc anchor relative times.
func Parse(text string, now time.Time, loc *time.Location) (When, string, error) {
	p := &parser{words: strings.Fields(text), now: now.In(loc), loc: loc}
	w, err := p.when()
	if err != nil {
		return When{}, "", err
	}
	if w.Rule.Once() {
		switch ahead := w.At.Sub(now); {
		case ahead < MinAhead:
			return When{}, "", errors.New("that's not in the future")
		case ahead > MaxAhead:
			return When{}, "", errors.New("that's more than a year away")
		}
	} else if w.Rule.Every > 0 && w.Rule.Every < MinInterval {
		return When{}, "", fmt.Errorf("repeats must be at least %s apart", formatDuration(MinInterval))
	}
	return w, strings.Join(p.words[p.i:], " "), nil
}

type parser struct {
	words []string
	i     int
	now   time.Time
	loc   *time.Location
}

func (p *parser) peek() string {
	if p.i < len(p.words) {
		return strings.ToLower(p.words[p.i])
	}
	return ""
}

func (p *parser) next() string { w := p.peek(); p.i++; return w }

func (p *parser) when() (When, error) {
	switch p.next() {
	case "in":
		d, ok := parseDuration(p.next())
		if !ok {
			return When{}, ErrUsage
		}
		return When{At: p.now.Add(d)}, nil
	case "at":
		h, m, err := p.clock()
		if err != nil {
			return When{}, err
		}
		at := p.day(0, h, m)
		if !at.After(p.now) {
			at = p.day(1, h, m)
		}
		return When{At: at}, nil
	case "tomorrow":
		h, m, err := p.optionalAt()
		if err != nil {
			return When{}, err
		}
		return When{At: p.day(1, h, m)}, nil
	case "on":
		return p.on()
	case "every":
		return p.every()
	case "daily":
		return p.daysAt([7]bool{true, true, true, true, true, true, true})
	}
	return When{}, ErrUsage
}

// day is today plus days, at h:m.
func (p *parser) day(days, h, m int) time.Time {
	return time.Date(p.now.Year(), p.now.Month(), p.now.Day()+days, h, m, 0, 0, p.loc)
}

func (p *parser) on() (When, error) {
	word := p.next()
	if d, ok := weekdays[word]; ok {
		h, m, err := p.optionalAt()
		if err != nil {
			return When{}, err
		}
		ahead := (int(d) - int(p.now.Weekday()) + 7) % 7
		if ahead == 0 {
			ahead = 7 // "on friday" on a Friday: next week's
		}
		return When{At: p.day(ahead, h, m)}, nil
	}
	year, month, day, hasYear, ok := parseDate(word)
	if !ok {
		return When{}, ErrUsage
	}
	h, m, err := p.optionalAt()
	if err != nil {
		return When{}, err
	}
	if !hasYear {
		year = p.now.Year()
	}
	at := time.Date(year, time.Month(month), day, h, m, 0, 0, p.loc)
	if at.Month() != time.Month(month) || at.Day() != day {
		return When{}, errors.New("there's no such date")
	}
	if !hasYear && !at.After(p.now) {
		at = at.AddDate(1, 0, 0) // the next one coming
	}
	return When{At: at}, nil
}

func (p *parser) every() (When, error) {
	word := p.next()
	if d, ok := parseDuration(word); ok {
		return When{At: p.now.Add(d), Rule: Rule{Every: d}}, nil
	}
	var days [7]bool
	switch word {
	case "day":
		days = [7]bool{true, true, true, true, true, true, true}
	case "weekday":
		days = [7]bool{false, true, true, true, true, true, false}
	case "weekend":
		days = [7]bool{true, false, false, false, false, false, true}
	default:
		for _, name := range strings.Split(word, ",") {
			d, ok := weekdays[name]
			if !ok {
				return When{}, ErrUsage
			}
			days[d] = true
		}
	}
	return p.daysAt(days)
}

func (p *parser) daysAt(days [7]bool) (When, error) {
	h, m, err := p.optionalAt()
	if err != nil {
		return When{}, err
	}
	r := Rule{Days: days, Hour: h, Minute: m}
	return When{At: r.Next(p.now, p.now, p.loc), Rule: r}, nil
}

// optionalAt reads "at <time>" if it's there; else the default 09:00.
func (p *parser) optionalAt() (h, m int, err error) {
	if p.peek() != "at" {
		return defaultHour, 0, nil
	}
	p.i++
	return p.clock()
}

// clock reads "18:30", "6:30pm", "6pm", or those with "am"/"pm" as a
// separate word. A bare hour ("6") is refused: it could be 06:00 or 18:00.
func (p *parser) clock() (h, m int, err error) {
	word := p.next()
	if s := p.peek(); s == "am" || s == "pm" {
		word += s
		p.i++
	}
	h, m, ok := parseClock(word)
	if !ok {
		return 0, 0, errors.New("write the time as 18:30 or 6:30pm")
	}
	return h, m, nil
}

func parseClock(s string) (h, m int, ok bool) {
	suffix := ""
	for _, x := range []string{"am", "pm"} {
		if rest, found := strings.CutSuffix(s, x); found {
			s, suffix = rest, x
		}
	}
	hs, ms, hasMinutes := strings.Cut(s, ":")
	if !hasMinutes && suffix == "" {
		return 0, 0, false // "6": ambiguous
	}
	h, err := strconv.Atoi(hs)
	if err != nil || len(hs) > 2 {
		return 0, 0, false
	}
	if hasMinutes {
		if m, err = strconv.Atoi(ms); err != nil || len(ms) != 2 || m > 59 {
			return 0, 0, false
		}
	}
	switch suffix {
	case "":
		if h > 23 {
			return 0, 0, false
		}
	default:
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		h %= 12
		if suffix == "pm" {
			h += 12
		}
	}
	return h, m, true
}

// parseDate reads "2026-12-24" or "12-24".
func parseDate(s string) (year, month, day int, hasYear, ok bool) {
	parts := strings.Split(s, "-")
	nums := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || part == "" || n < 0 {
			return 0, 0, 0, false, false
		}
		nums[i] = n
	}
	switch {
	case len(nums) == 3 && len(parts[0]) == 4:
		return nums[0], nums[1], nums[2], true, valid(nums[1], nums[2])
	case len(nums) == 2:
		return 0, nums[0], nums[1], false, valid(nums[0], nums[1])
	}
	return 0, 0, 0, false, false
}

func valid(month, day int) bool { return month >= 1 && month <= 12 && day >= 1 && day <= 31 }

// parseDuration reads "45m", "2h", "1h30m", "3d", "2w": numbers with units
// m, h, d, w, at least one.
func parseDuration(s string) (time.Duration, bool) {
	units := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	var total time.Duration
	n, digits := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if digits++; digits > 6 {
				return 0, false
			}
			n = n*10 + int(c-'0')
			continue
		}
		unit, ok := units[c]
		if !ok || digits == 0 {
			return 0, false
		}
		total += time.Duration(n) * unit
		n, digits = 0, 0
	}
	if digits != 0 || total <= 0 {
		return 0, false
	}
	return total, true
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// formatDuration is "2h", "1h 30m", "3d".
func formatDuration(d time.Duration) string {
	var parts []string
	for _, u := range []struct {
		d    time.Duration
		name string
	}{{7 * 24 * time.Hour, "w"}, {24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if n := d / u.d; n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, u.name))
			d -= n * u.d
		}
	}
	return strings.Join(parts, " ")
}

// Usage is the short help shown when a when isn't understood.
const Usage = "Tell me when, e.g. `in 2h`, `at 18:30`, `tomorrow at 9am`, `on friday at 20:00`, " +
	"`on 2026-12-24`, `every 2h`, `every day at 08:00`, `every weekday at 9:00`, `every mon,thu at 19:00`."
