// Package spotify turns a Spotify track link into the track's title, artists
// and length, without an API key: it reads the public track page's meta tags.
// Only metadata is read; audio always comes from YouTube.
package spotify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Link parsing errors.
var (
	ErrNotSpotify = errors.New("not a Spotify link")
	ErrNotTrack   = errors.New("only single Spotify tracks are supported")
	ErrShortLink  = errors.New("short spotify.link links aren't supported")
	ErrNotFound   = errors.New("Spotify track not found")
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)

// IsLink reports whether raw is meant as a Spotify link (of any kind).
func IsLink(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(s, "spotify:") || strings.Contains(s, "open.spotify.com") || strings.Contains(s, "spotify.link")
}

// ParseTrackID extracts the track ID from a Spotify track link. Accepted forms:
// open.spotify.com/track/<id> (optionally /intl-xx/track/<id>), with or without
// a scheme, query or <...> wrapping; and spotify:track:<id>.
func ParseTrackID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")

	const uriPrefix = "spotify:"
	if len(s) > len(uriPrefix) && strings.EqualFold(s[:len(uriPrefix)], uriPrefix) {
		kind, id, _ := strings.Cut(s[len(uriPrefix):], ":")
		switch {
		case !strings.EqualFold(kind, "track"):
			return "", ErrNotTrack
		case !idPattern.MatchString(id):
			return "", ErrNotSpotify
		}
		return id, nil
	}

	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ErrNotSpotify
	}
	switch strings.ToLower(u.Hostname()) {
	case "open.spotify.com":
	case "spotify.link":
		return "", ErrShortLink
	default:
		return "", ErrNotSpotify
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && strings.HasPrefix(strings.ToLower(parts[0]), "intl-") {
		parts = parts[1:]
	}
	if len(parts) != 2 {
		return "", ErrNotSpotify
	}
	if !strings.EqualFold(parts[0], "track") { // like spotify: URIs, ignore case
		return "", ErrNotTrack
	}
	if !idPattern.MatchString(parts[1]) {
		return "", ErrNotSpotify
	}
	return parts[1], nil
}

// Track is what a Spotify track page tells us.
type Track struct {
	ID       string
	Title    string        // e.g. "Uptown Funk (feat. Bruno Mars)"
	Artists  string        // comma-separated, e.g. "Mark Ronson, Bruno Mars"
	Duration time.Duration // 0 if unknown
}

// Name is "Artists – Title".
func (t Track) Name() string {
	if t.Artists == "" {
		return t.Title
	}
	return t.Artists + " – " + t.Title
}

// ArtistList splits Artists on commas.
func (t Track) ArtistList() []string {
	var out []string
	for _, a := range strings.Split(t.Artists, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// DefaultBaseURL is where track pages live.
const DefaultBaseURL = "https://open.spotify.com"

// maxPageBytes caps how much of a track page is read (real pages are ~300 KB).
const maxPageBytes = 2 << 20

// Client reads public Spotify track pages.
type Client struct {
	HTTP    *http.Client // nil = a client with a 10 s timeout
	BaseURL string       // "" = DefaultBaseURL; tests point it at a fake server
}

// Track fetches and parses the page for track id. Only a URL built from a
// validated ID is ever fetched.
func (c *Client) Track(ctx context.Context, id string) (Track, error) {
	if !idPattern.MatchString(id) {
		return Track{}, fmt.Errorf("invalid Spotify track id %q", id)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/track/"+id, nil)
	if err != nil {
		return Track{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; vox-engine)")
	req.Header.Set("Accept-Language", "en")
	resp, err := hc.Do(req)
	if err != nil {
		return Track{}, fmt.Errorf("fetch Spotify track page: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Track{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Track{}, fmt.Errorf("fetch Spotify track page: HTTP %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return Track{}, fmt.Errorf("read Spotify track page: %w", err)
	}
	t, err := parsePage(string(page))
	t.ID = id
	return t, err
}

var metaPattern = regexp.MustCompile(`<meta\s+(?:property|name)="([^"]+)"\s+content="([^"]*)"`)

// parsePage reads the track's meta tags.
func parsePage(page string) (Track, error) {
	meta := map[string]string{}
	for _, m := range metaPattern.FindAllStringSubmatch(page, -1) {
		if _, seen := meta[m[1]]; !seen {
			meta[m[1]] = html.UnescapeString(m[2])
		}
	}
	if meta["og:type"] != "music.song" || meta["og:title"] == "" {
		return Track{}, errors.New("Spotify page has no track details (layout changed?)")
	}
	t := Track{Title: strings.TrimSpace(meta["og:title"]), Artists: strings.TrimSpace(meta["music:musician_description"])}
	if t.Artists == "" {
		// og:description is "Artists · Album · Song · Year".
		t.Artists, _, _ = strings.Cut(meta["og:description"], " · ")
		t.Artists = strings.TrimSpace(t.Artists)
	}
	if secs, err := strconv.Atoi(meta["music:duration"]); err == nil && secs > 0 {
		t.Duration = time.Duration(secs) * time.Second
	}
	return t, nil
}
