// Package steamaccel provides a bounded, HTTP-only Steam content relay across
// two independently scheduled uplinks. It does not bond individual TCP streams.
package steamaccel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type DialContextFunc func(context.Context, string, string) (net.Conn, error)
type LookupIPFunc func(context.Context, string) ([]net.IP, error)

// Config contains production settings and trusted test/integration seams.
// See API.md for defaults, DNS override caveats and seam security obligations.
type Config struct {
	// MaxPhoneConnections bounds dialing, active and idle phone sockets (default/max 32).
	MaxPhoneConnections                                                      int
	HomeIPv4                                                                 string
	AllowHomePeer                                                            bool
	PhoneProxyAddr                                                           string
	PhoneToken                                                               string
	HomeWeight, PhoneWeight                                                  int
	AllowedHosts                                                             []string
	MaxConcurrent, MaxHeaderBytes                                            int
	HeaderTimeout, IdleTimeout, DialTimeout, RequestTimeout, FailureCooldown time.Duration
	LookupIP                                                                 LookupIPFunc
	HomeDialContext, PhoneDialContext                                        DialContextFunc
	HomeTransport, PhoneTransport                                            http.RoundTripper
}

func defaults(c Config) (Config, error) {
	ip := net.ParseIP(c.HomeIPv4)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return c, errors.New("HomeIPv4 must be a specific IPv4 address assigned to the home interface")
	}
	c.HomeIPv4 = ip.To4().String()
	if c.PhoneProxyAddr == "" {
		c.PhoneProxyAddr = "127.0.0.1:8283"
	}
	host, port, err := net.SplitHostPort(c.PhoneProxyAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || !validPort(port) {
		return c, errors.New("PhoneProxyAddr must be a numeric loopback IP and valid port")
	}
	if c.PhoneToken == "" || len(c.PhoneToken) > 4096 || !validBearerToken(c.PhoneToken) {
		return c, errors.New("PhoneToken must be a nonempty printable ASCII bearer token without whitespace (max 4096 bytes)")
	}
	if c.HomeWeight == 0 && c.PhoneWeight == 0 {
		c.HomeWeight, c.PhoneWeight = 1, 4
	}
	if c.HomeWeight < 1 || c.PhoneWeight < 1 || c.HomeWeight > 1000000 || c.PhoneWeight > 1000000 {
		return c, errors.New("uplink weights must be in 1..1000000")
	}
	if c.AllowedHosts == nil {
		c.AllowedHosts = []string{"steamcontent.com", "steampipe.akamaized.net"}
	}
	if len(c.AllowedHosts) == 0 {
		return c, errors.New("AllowedHosts cannot be empty")
	}
	hosts := make([]string, len(c.AllowedHosts))
	for i, h := range c.AllowedHosts {
		h = strings.ToLower(h)
		if !validDNSName(h) {
			return c, fmt.Errorf("invalid AllowedHosts entry %q", h)
		}
		hosts[i] = h
	}
	c.AllowedHosts = hosts
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 64
	}
	if c.MaxPhoneConnections == 0 {
		c.MaxPhoneConnections = 32
	}
	if c.MaxPhoneConnections < 1 || c.MaxPhoneConnections > 32 {
		return c, errors.New("MaxPhoneConnections must be in 1..32")
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = 32 << 10
	}
	if c.HeaderTimeout == 0 {
		c.HeaderTimeout = 15 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 30 * time.Minute
	}
	if c.FailureCooldown == 0 {
		c.FailureCooldown = 5 * time.Second
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 4096 || c.MaxHeaderBytes < 1024 || c.MaxHeaderBytes > 1<<20 {
		return c, errors.New("invalid concurrency or header limit")
	}
	if c.HeaderTimeout < 0 || c.IdleTimeout < 0 || c.DialTimeout < 0 || c.RequestTimeout < 0 || c.FailureCooldown < 0 {
		return c, errors.New("timeouts and cooldown must be positive")
	}
	if c.LookupIP == nil {
		c.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	return c, nil
}
