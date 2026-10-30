package mediafile

import (
	"net/netip"
	"net/url"
	"path"
	"strings"
)

// CheckLink turns a play argument into the link to fetch: http or https
// (assumed when no scheme is given), with a host, optionally wrapped in <...>
// (Discord's no-embed syntax). Plainly local hosts (localhost, a private IP)
// are refused at once; the client still checks every address it dials.
func CheckLink(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", &Error{Kind: KindBadLink, Detail: "not an http(s) link"}
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", &Error{Kind: KindBlocked, Detail: "host " + host}
	}
	if a, err := netip.ParseAddr(host); err == nil && !Public(netip.AddrPortFrom(a, 0)) {
		return "", &Error{Kind: KindBlocked, Detail: "host " + host}
	}
	return u.String(), nil
}

// mediaExts are file extensions taken as audio or video when the type is
// missing or generic.
var mediaExts = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".flac": true, ".ogg": true, ".oga": true,
	".opus": true, ".wma": true, ".aif": true, ".aiff": true, ".alac": true, ".ape": true, ".wv": true,
	".mka": true, ".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true,
	".wmv": true, ".flv": true, ".3gp": true, ".ts": true, ".mpg": true, ".mpeg": true, ".amr": true,
}

// IsMedia reports whether an attachment looks like audio or video: by its
// content type, or, when that's missing or generic, by its extension.
func IsMedia(name, contentType string) bool {
	t, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	t = strings.TrimSpace(t)
	switch {
	case strings.HasPrefix(t, "audio/"), strings.HasPrefix(t, "video/"), t == "application/ogg":
		return true
	case t == "", t == "application/octet-stream":
		return mediaExts[strings.ToLower(path.Ext(name))]
	}
	return false
}
