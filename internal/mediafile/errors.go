package mediafile

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Kind classifies why a file couldn't be played.
type Kind int

const (
	KindFailed      Kind = iota // anything unrecognised
	KindUnreachable             // DNS, connection or TLS failure
	KindHTTP                    // the server answered with an error status
	KindBlocked                 // the link leads to a private or local address
	KindPage                    // a web page, not a file
	KindNotMedia                // some other kind of file (text, archive, ...)
	KindTooBig                  // over max_file_bytes
	KindTooLong                 // over max_duration_seconds
	KindNoAudio                 // ffmpeg found no audio it could convert
	KindTimeout                 // the download or conversion took too long
	KindMissingTool             // ffmpeg not found
	KindBadLink                 // not an http(s) link
)

var kindNames = [...]string{
	KindFailed: "failed", KindUnreachable: "unreachable", KindHTTP: "http-error", KindBlocked: "blocked",
	KindPage: "web-page", KindNotMedia: "not-media", KindTooBig: "too-big", KindTooLong: "too-long",
	KindNoAudio: "no-audio", KindTimeout: "timeout", KindMissingTool: "missing-tool", KindBadLink: "bad-link",
}

// String is a short name for logs, e.g. "too-big".
func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "unknown"
}

// Error is a classified failure. Detail is for logs; UserMessage is for Discord.
type Error struct {
	Kind     Kind
	Detail   string
	Status   int           // for KindHTTP
	MaxBytes int64         // for KindTooBig
	MaxLen   time.Duration // for KindTooLong
}

func (e *Error) Error() string { return fmt.Sprintf("file %s: %s", e.Kind, e.Detail) }

// UserMessage is a short, human-readable explanation for the requester.
func (e *Error) UserMessage() string {
	switch e.Kind {
	case KindUnreachable:
		return "I couldn't reach that link."
	case KindHTTP:
		return fmt.Sprintf("The link answered with an error (HTTP %d).", e.Status)
	case KindBlocked:
		return "That link leads to a private or local address, so I won't fetch it."
	case KindPage:
		return "That link is a web page, not an audio or video file. Link straight to the file."
	case KindNotMedia:
		return "That isn't an audio or video file."
	case KindTooBig:
		return fmt.Sprintf("That file is over the %s limit.", FormatBytes(e.MaxBytes))
	case KindTooLong:
		return fmt.Sprintf("That file is longer than the %s limit.", formatLength(e.MaxLen))
	case KindNoAudio:
		return "That file has no playable audio (it may be damaged, or in a format I can't read)."
	case KindTimeout:
		return "The file took too long to download or convert, so I gave up."
	case KindMissingTool:
		return "The bot can't convert files right now (ffmpeg is missing on its machine)."
	case KindBadLink:
		return "That doesn't look like a YouTube video link or a link to a file."
	default:
		return "Something went wrong with that file. Details are in the bot's log."
	}
}

// FormatBytes renders n as megabytes to one decimal, e.g. "50 MB" or "7.5 MB"
// (1 MB = 1 MiB).
func FormatBytes(n int64) string {
	mb := math.Round(float64(n)/(1<<20)*10) / 10
	return strconv.FormatFloat(mb, 'f', -1, 64) + " MB"
}

func formatLength(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d-minute", int(d.Minutes()))
	}
	return d.String()
}
