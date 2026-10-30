package mediafile

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// maxRedirects is how many redirects a link may take.
const maxRedirects = 3

// errBlocked is returned when a connection would reach a non-public address.
var errBlocked = errors.New("address is not public")

// NewClient returns the HTTP client for user-supplied links. It connects only
// to public addresses: the check runs on the address actually dialled, after
// DNS, so a name that resolves to a private address (or a redirect to one) is
// refused too. Proxies from the environment are ignored.
func NewClient() *http.Client { return newClient(Public) }

// newClient is NewClient with allow deciding which addresses may be dialled.
func newClient(allow func(netip.AddrPort) bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		// address is the resolved "ip:port" about to be dialled.
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !allow(ap) {
			return fmt.Errorf("%w: %s", errBlocked, address)
		}
		return nil
	}}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: checkRedirect,
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("more than %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to a %q link", req.URL.Scheme)
	}
	return nil
}

// nonPublic are special-purpose ranges netip's methods don't cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: could reach any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: embeds an IPv4 address
}

// Public reports whether ap's address is an ordinary internet address: not
// loopback, private, link-local, multicast, unspecified or otherwise special.
// IPv4-mapped IPv6 addresses are judged as the IPv4 address they carry.
func Public(ap netip.AddrPort) bool {
	a := ap.Addr().Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
