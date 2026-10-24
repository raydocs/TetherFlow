package steamaccel

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

func validPort(s string) bool {
	if s == "" {
		return false
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return false
		}
	}
	p, err := strconv.Atoi(s)
	return err == nil && p > 0 && p <= 65535
}

func validDNSName(s string) bool {
	if len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) < 1 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, b := range []byte(l) {
			if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
				return false
			}
		}
	}
	// Numeric-looking hosts must not be interpreted differently by downstream stacks.
	numeric := true
	for _, b := range []byte(labels[len(labels)-1]) {
		if b < '0' || b > '9' {
			numeric = false
		}
	}
	return !numeric
}

func (r *Relay) authority(raw string) (string, string, error) {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n/@\\?#%") {
		return "", "", errors.New("invalid Host")
	}
	host, port := raw, "80"
	if strings.Contains(raw, ":") {
		var err error
		host, port, err = net.SplitHostPort(raw)
		if err != nil {
			return "", "", errors.New("invalid Host port")
		}
	}
	host = strings.ToLower(host)
	if !validDNSName(host) || (port != "80" && port != "443") {
		return "", "", errors.New("Host or port forbidden")
	}
	for _, allowed := range r.cfg.AllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return host, port, nil
		}
	}
	return "", "", errors.New("Host not allowlisted")
}

// Conservative deny ranges include special-purpose, documentation, translation,
// multicast and reserved space, not just RFC1918. Unknown/non-IP answers fail shut.
var deniedPrefixes = func() []netip.Prefix {
	ranges := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	out := make([]netip.Prefix, 0, len(ranges))
	for _, s := range ranges {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	// IPv6 currently routable unicast allocation; exclude alternate encodings.
	if a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a) {
		return false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (r *Relay) peerAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || r.cfg.AllowHomePeer && ip.Equal(net.ParseIP(r.cfg.HomeIPv4)))
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func cleanHeaders(src http.Header) http.Header {
	denied := make(map[string]bool, len(hopHeaders))
	for _, h := range hopHeaders {
		denied[strings.ToLower(h)] = true
	}
	for k, values := range src {
		if strings.EqualFold(k, "Connection") {
			for _, value := range values {
				for _, h := range strings.Split(value, ",") {
					denied[strings.ToLower(strings.TrimSpace(h))] = true
				}
			}
		}
	}
	dst := make(http.Header, len(src))
	for k, values := range src {
		if !denied[strings.ToLower(k)] {
			dst[http.CanonicalHeaderKey(k)] = append(dst[http.CanonicalHeaderKey(k)], values...)
		}
	}
	return dst
}

func validBearerToken(token string) bool {
	for _, b := range []byte(token) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func headerSize(h http.Header) int {
	size := 2
	for k, vs := range h {
		for _, v := range vs {
			size += len(k) + len(v) + 4
		}
	}
	return size
}
