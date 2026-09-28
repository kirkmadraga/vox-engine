package ytdlp

import (
	"fmt"
	"strings"
	"time"
)

// Kind classifies a download failure.
type Kind int

const (
	KindFailed        Kind = iota // anything unrecognised
	KindInvalid                   // bad video ID
	KindPrivate                   // private video
	KindAgeRestricted             // needs sign-in to confirm age
	KindRegionLocked              // not available in the server's country
	KindMembersOnly               // channel members only
	KindUnavailable               // deleted, removed, or otherwise gone
	KindLive                      // live stream or upcoming premiere
	KindTooLong                   // over max_duration_seconds
	KindBotCheck                  // YouTube demands sign-in to prove we're not a bot
	KindTimeout                   // yt-dlp took too long
	KindMissingTool               // yt-dlp not installed / not found
)

// Error is a classified yt-dlp failure. Detail is for logs; UserMessage is for Discord.
type Error struct {
	Kind   Kind
	Detail string
	Limit  time.Duration // for KindTooLong
}

func (e *Error) Error() string { return fmt.Sprintf("yt-dlp: %s", e.Detail) }

// UserMessage is a short, human-readable explanation for the requester.
func (e *Error) UserMessage() string {
	switch e.Kind {
	case KindInvalid:
		return "That doesn't look like a valid YouTube video."
	case KindPrivate:
		return "That video is private."
	case KindAgeRestricted:
		return "That video is age-restricted, so I can't play it."
	case KindRegionLocked:
		return "That video isn't available in the bot's region."
	case KindMembersOnly:
		return "That video is for channel members only."
	case KindUnavailable:
		return "That video is unavailable (it may have been deleted)."
	case KindLive:
		return "Live streams and upcoming premieres aren't supported."
	case KindTooLong:
		return fmt.Sprintf("That video is longer than the %s limit.", formatDuration(e.Limit))
	case KindBotCheck:
		return "YouTube is blocking downloads from the bot right now. Try again later."
	case KindTimeout:
		return "The download took too long, so I gave up."
	case KindMissingTool:
		return "The bot can't download right now (yt-dlp is missing on its machine)."
	default:
		return "The download failed. Details are in the bot's log."
	}
}

func formatDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d-minute", int(d.Minutes()))
	}
	return d.String()
}

// classify maps yt-dlp's stderr to a Kind. Order matters: region messages also
// contain "Video unavailable", so specific checks come before generic ones.
func classify(stderr string, runErr error) *Error {
	s := strings.ToLower(stderr)
	kind := KindFailed
	switch {
	case strings.Contains(s, "not a bot"):
		kind = KindBotCheck
	case strings.Contains(s, "private video"):
		kind = KindPrivate
	case strings.Contains(s, "confirm your age"), strings.Contains(s, "age-restricted"), strings.Contains(s, "inappropriate for some users"):
		kind = KindAgeRestricted
	case strings.Contains(s, "in your country"), strings.Contains(s, "geo restrict"), strings.Contains(s, "geo-restrict"):
		kind = KindRegionLocked
	case strings.Contains(s, "members-only"), strings.Contains(s, "join this channel"), strings.Contains(s, "channel's members"):
		kind = KindMembersOnly
	case strings.Contains(s, "live event will begin"), strings.Contains(s, "premieres in"), strings.Contains(s, "is live"), strings.Contains(s, "is currently live"):
		kind = KindLive
	case strings.Contains(s, "video unavailable"), strings.Contains(s, "has been removed"), strings.Contains(s, "been terminated"), strings.Contains(s, "no longer available"), strings.Contains(s, "does not exist"):
		kind = KindUnavailable
	}
	return &Error{Kind: kind, Detail: fmt.Sprintf("%v: %s", runErr, lastErrorLine(stderr))}
}

// lastErrorLine picks the most useful stderr line for logs.
func lastErrorLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "ERROR:") {
			return strings.TrimSpace(lines[i])
		}
	}
	return truncate(strings.TrimSpace(stderr), 500)
}
