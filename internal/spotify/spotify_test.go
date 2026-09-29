package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const rickID = "4PTG3Z6ehGkBFwjybzWkR8"

func TestParseTrackID(t *testing.T) {
	for _, in := range []string{
		"https://open.spotify.com/track/" + rickID,
		"https://open.spotify.com/track/" + rickID + "?si=abc123",
		"http://open.spotify.com/track/" + rickID,
		"open.spotify.com/track/" + rickID,
		"<https://open.spotify.com/track/" + rickID + ">",
		"https://open.spotify.com/intl-de/track/" + rickID,
		"HTTPS://OPEN.SPOTIFY.COM/track/" + rickID,
		"spotify:track:" + rickID,
		"  spotify:track:" + rickID + "  ",
	} {
		if id, err := ParseTrackID(in); err != nil || id != rickID {
			t.Errorf("ParseTrackID(%q) = %q, %v", in, id, err)
		}
	}
}

func TestParseTrackIDRejects(t *testing.T) {
	cases := map[string]error{
		"https://open.spotify.com/album/6eUW0wxWtzkFdaEFsTJto6":     ErrNotTrack,
		"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M":  ErrNotTrack,
		"https://open.spotify.com/artist/0gxyHStUsqpMadRV0Di1Qt":    ErrNotTrack,
		"spotify:album:6eUW0wxWtzkFdaEFsTJto6":                      ErrNotTrack,
		"https://spotify.link/AbCdEf":                               ErrShortLink,
		"https://open.spotify.com/track/short":                      ErrNotSpotify,
		"https://open.spotify.com/track/" + rickID + "/extra":       ErrNotSpotify,
		"https://open.spotify.com/track/../../etc/passwd1234567890": ErrNotSpotify,
		"spotify:track:bad":                                     ErrNotSpotify,
		"https://evil.example/track/" + rickID:                  ErrNotSpotify,
		"https://open.spotify.com.evil.example/track/" + rickID: ErrNotSpotify,
		"ftp://open.spotify.com/track/" + rickID:                ErrNotSpotify,
	}
	for in, want := range cases {
		if id, err := ParseTrackID(in); !errors.Is(err, want) {
			t.Errorf("ParseTrackID(%q) = %q, %v; want %v", in, id, err, want)
		}
	}
}

func TestIsLink(t *testing.T) {
	for in, want := range map[string]bool{
		"https://open.spotify.com/track/" + rickID: true,
		"spotify:track:" + rickID:                  true,
		"https://spotify.link/x":                   true,
		"never gonna give you up":                  false,
		"spotify wrapped 2025":                     false,
		"https://youtu.be/dQw4w9WgXcQ":             false,
	} {
		if IsLink(in) != want {
			t.Errorf("IsLink(%q) = %v", in, !want)
		}
	}
}

// page mimics the meta tags of a real open.spotify.com track page (2026-09).
func page(title, desc, musicians, duration string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><title>x</title>`)
	b.WriteString(`<meta property="og:site_name" content="Spotify"/>`)
	b.WriteString(`<meta property="og:title" content="` + title + `"/>`)
	b.WriteString(`<meta property="og:description" content="` + desc + `"/>`)
	b.WriteString(`<meta property="og:type" content="music.song"/>`)
	if duration != "" {
		b.WriteString(`<meta name="music:duration" content="` + duration + `"/>`)
	}
	if musicians != "" {
		b.WriteString(`<meta name="music:musician_description" content="` + musicians + `"/>`)
	}
	b.WriteString(`</head><body>` + strings.Repeat("x", 1000) + `</body></html>`)
	return b.String()
}

// server serves body at /track/<rickID> with status.
func server(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/track/"+rickID || r.Header.Get("User-Agent") == "" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}
}

func TestTrack(t *testing.T) {
	c := server(t, 200, page("Uptown Funk (feat. Bruno Mars)", "Mark Ronson, Bruno Mars · Uptown Special · Song · 2015", "Mark Ronson, Bruno Mars", "270"))
	got, err := c.Track(context.Background(), rickID)
	want := Track{ID: rickID, Title: "Uptown Funk (feat. Bruno Mars)", Artists: "Mark Ronson, Bruno Mars", Duration: 270 * time.Second}
	if err != nil || got != want {
		t.Fatalf("Track = %+v, %v", got, err)
	}
	if got.Name() != "Mark Ronson, Bruno Mars – Uptown Funk (feat. Bruno Mars)" {
		t.Errorf("Name = %q", got.Name())
	}
	if a := got.ArtistList(); len(a) != 2 || a[0] != "Mark Ronson" || a[1] != "Bruno Mars" {
		t.Errorf("ArtistList = %q", a)
	}
}

func TestTrackUnescapesAndFallsBack(t *testing.T) {
	// No musician tag: artists come from og:description. HTML entities are decoded.
	c := server(t, 200, page("Rock &amp; Roll &quot;Live&quot;", "Simon &amp; Garfunkel · Album · Song · 1970", "", ""))
	got, err := c.Track(context.Background(), rickID)
	if err != nil || got.Title != `Rock & Roll "Live"` || got.Artists != "Simon & Garfunkel" || got.Duration != 0 {
		t.Errorf("Track = %+v, %v", got, err)
	}
}

func TestTrackErrors(t *testing.T) {
	cases := map[string]struct {
		c    *Client
		want string
	}{
		"not found":      {server(t, 404, ""), ErrNotFound.Error()},
		"server error":   {server(t, 500, ""), "HTTP 500"},
		"not a track":    {server(t, 200, `<meta property="og:type" content="music.album"/><meta property="og:title" content="A"/>`), "no track details"},
		"layout changed": {server(t, 200, "<html>nothing here</html>"), "no track details"},
	}
	for name, c := range cases {
		if _, err := c.c.Track(context.Background(), rickID); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := (&Client{BaseURL: "http://127.0.0.1:1"}).Track(context.Background(), "../../etc/passwd"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("bad id: err = %v", err)
	}
}

func TestTrackTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := c.Track(context.Background(), rickID); err == nil {
		t.Error("want a timeout error")
	}
}

// A huge page is cut off at maxPageBytes; tags past the cut are not seen.
func TestTrackCapsPageSize(t *testing.T) {
	body := strings.Repeat("x", maxPageBytes) + page("T", "A · B · Song · 1", "A", "1")
	if _, err := server(t, 200, body).Track(context.Background(), rickID); err == nil || !strings.Contains(err.Error(), "no track details") {
		t.Errorf("err = %v", err)
	}
}
