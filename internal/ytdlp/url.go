package ytdlp

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// URL parsing errors, phrased for users.
var (
	ErrNotYouTube = errors.New("that doesn't look like a YouTube video link")
	ErrPlaylist   = errors.New("playlists aren't supported, only single videos")
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ValidID reports whether id has the shape of a YouTube video ID. IDs are used
// as file names, so this also guards against path tricks.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// VideoURL is the canonical URL for id. Only this, never user text, is passed
// to yt-dlp.
func VideoURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

// LooksLikeLink reports whether raw is meant as a link rather than search
// words: it has a scheme, mentions a YouTube domain, or is a single word with
// a dot and a slash (e.g. "vimeo.com/1").
func LooksLikeLink(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(s, "://"), strings.Contains(s, "youtube.com"), strings.Contains(s, "youtu.be"):
		return true
	}
	return !strings.ContainsFunc(s, unicode.IsSpace) && strings.Contains(s, ".") && strings.Contains(s, "/")
}

// ParseVideoID extracts the video ID from a YouTube link. Accepted forms:
// youtu.be/<id>, youtube.com/watch?v=<id>, youtube.com/shorts/<id>, on the
// www, m and music subdomains, with or without a scheme, and wrapped in <...>
// (Discord's no-embed syntax). Extra query parameters, including a playlist
// alongside a video, are ignored.
func ParseVideoID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ErrNotYouTube
	}

	host := strings.ToLower(u.Hostname())
	path := strings.Trim(u.Path, "/")
	var id string
	switch host {
	case "youtu.be":
		id = path
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		switch {
		case path == "watch":
			id = u.Query().Get("v")
		case strings.HasPrefix(path, "shorts/"):
			id = strings.TrimPrefix(path, "shorts/")
		case path == "playlist":
			return "", ErrPlaylist
		}
	default:
		return "", ErrNotYouTube
	}
	if !ValidID(id) {
		return "", ErrNotYouTube
	}
	return id, nil
}
