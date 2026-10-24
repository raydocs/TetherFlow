# Steam HTTP and opaque TLS relay core

See [API.md](API.md) for the stable embedding API, defaults, resolver/dial test
seams and launcher responsibilities. No CLI, hosts installer or PAC changes live
here. No external Go dependencies.

This schedules independent GET/HEAD transfers across source-bound home Wi-Fi and
an authenticated local iPhone CONNECT proxy. It is **not single-flow bonding**.
Only local peers are admitted; `AllowHomePeer` additionally permits the explicitly
configured home IPv4 source for Steam's LAN cache discovery scenario.

Security checks run before either transport: strict suffix allowlist, port 80/443,
public-only DNS (including rejection of mixed public/private answers), pinned
numeric destinations and bidirectional hop-header stripping. Response headers,
CONNECT handshakes, active requests and copy buffers are bounded. Only
pre-response failures may fail over; interrupted bodies abort rather than append
a second origin response. Downstream cancellation does not damage path health.
Use `Relay.Server` or enforce equivalent HTTP parser/deadline bounds yourself.
The phone has a separate total socket budget (default/max 32, optionally smaller
via `MaxPhoneConnections`), including pending dials and idle keepalive pools.
Local capacity exhaustion does not cool down the phone; idle-host eviction and
home fallback handle CDN rotation without disabling keepalive.

Opaque TLS ingress uses `Relay.ServeTLS(net.Listener) error`; the launcher binds
the exact local HomeIPv4:443. No certificates are installed and no TLS is
terminated. Read-only ClientHello capture is limited to 64 KiB / HeaderTimeout;
missing/invalid SNI, ECH and non-allowlisted/private origins fail closed without
TLS alerts. Captured bytes pass unchanged to numeric pinned port 443 over either
uplink (authenticated CONNECT443 on phone). HTTP and TLS share concurrency,
scheduling and the phone socket budget. Only dial/CONNECT or zero-byte initial
write failures may fall back; partial TLS streams are never replayed. Bounded
bidirectional copies use shared idle and maximum-lifetime limits, preserve TCP
half-close where supported (safe full-close otherwise), and listener exit or
Relay.Close cancels and waits for sessions. TLS status Bytes are downstream
encrypted wire bytes, not HTTP body bytes.

**Limitations:** no TLS MITM or arbitrary TLS interception; modern
Steam cache discovery/adoption is unproven. A hosts/DNS override needs an
independent public-origin `LookupIP` resolver to avoid intentionally rejected
local answers. Home sockets require an IPv4 origin (phone may use public IPv6).
The phone needs CONNECT80 for HTTP caches and CONNECT443 for opaque TLS;
CONNECT443-only cannot carry HTTP ingress cache downloads.
Windows interface selection is implemented but not physically verified. DNS/hosts
setup, USB reachability, iPhone foreground/cellular routing, correct Wi-Fi egress,
Steam adoption and actual speed gains require real Windows+iPhone evidence.

Checks, from `desktop/`:

```sh
go test -race ./internal/steamaccel -count=3
go vet ./internal/steamaccel
GOOS=windows GOARCH=amd64 go test ./internal/steamaccel -run '^$' -exec=/usr/bin/true
```

The last command is a compile check only, not Windows execution evidence. Tests
use real httptest origins and authenticated CONNECT tunnels, with trusted dial
mapping to local servers while retaining production public-DNS checks.
