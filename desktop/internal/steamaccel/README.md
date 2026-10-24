# Steam HTTP relay core

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

**Limitations:** HTTP ingress only, no TLS interception/SNI passthrough; modern
Steam cache discovery/adoption is unproven. A hosts/DNS override needs an
independent public-origin `LookupIP` resolver to avoid intentionally rejected
local answers. Home sockets require an IPv4 origin (phone may use public IPv6).
The phone proxy must support CONNECT80; CONNECT443-only cannot carry HTTP caches.
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
