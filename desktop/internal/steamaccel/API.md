# Steam HTTP and opaque TLS dual-uplink relay API (Go 1.22)

```go
r, err := steamaccel.New(steamaccel.Config{
    HomeIPv4: "192.168.1.10", // REQUIRED: assigned Wi-Fi IPv4; home sockets bind it
    AllowHomePeer: true,       // permit this exact source IP as well as loopback
    PhoneProxyAddr: "127.0.0.1:8283", // default
    PhoneToken: token,         // REQUIRED unless Uplinks is UplinkHomeOnly; sent only to local phone CONNECT proxy
    Uplinks: steamaccel.UplinkDual, // zero value; or UplinkHomeOnly / UplinkPhoneOnly
    HomeWeight: 1, PhoneWeight: 4, // zero/zero selects these defaults
})
if err != nil { /* handle */ }
defer r.Close()
s := r.Server("192.168.1.10:80") // bounded HTTP server; call ListenAndServe yourself
// Optional opaque TLS ingress: caller binds exact HomeIPv4:443, then r.ServeTLS(listener).
// r also implements http.Handler (GET/HEAD only).
// r.Snapshot() returns Status{Home, Phone PathStatus; Mode UplinkMode; PhoneAuth PhoneAuthStatus}.
// r.SetPhoneToken(typedToken) swaps the phone bearer token while running (see Phone token refresh).
```

Stable public surface: `New(Config) (*Relay, error)`, `(*Relay).ServeHTTP`,
`(*Relay).Server(addr string) *http.Server`, `(*Relay).ServeTLS(net.Listener) error`,
`(*Relay).Snapshot() Status`, `(*Relay).SetPhoneToken(string) error`, `NormalizePhoneToken(string) (string, error)`,
`(*Relay).Close() error`. `PathStatus` exposes `Requests`, `Active`, `Bytes`,
`Errors` (uint64), `CooldownUntil` (time.Time); `Status` exposes `Home`, `Phone`
and `Mode` (the configured `UplinkMode`).
For HTTP, Bytes mean downstream response-body bytes, added as each write to the
client completes (not when the response finishes), so a running transfer is visible
in `Snapshot`. A partial write counts the bytes it wrote. For opaque TLS, Bytes mean
encrypted downstream wire bytes, also added per write. Finishing a request adds no
bytes, so nothing is counted twice.
Requests count outbound attempts. A canceled inbound request does not count as a
path error. Weights are weighted least-outstanding, not bandwidth bonding.

Config (all optional except HomeIPv4, and PhoneToken unless `Uplinks` is `UplinkHomeOnly`):
- `Uplinks UplinkMode`: `UplinkDual` (zero value, both paths with failover),
  `UplinkHomeOnly` or `UplinkPhoneOnly` (`String()` gives `dual`, `homeonly`,
  `phoneonly`). Any other value is rejected by `New`. `acquire` never selects a
  disabled path, so a single-path mode has no failover and no same-path retry:
  a failure answers 502 (or 503 while the only path cools down) / closes the TLS
  session. `UplinkHomeOnly` needs no `PhoneToken` (one that is supplied must still
  be well formed), builds no phone transport and never dials, reserves a socket
  for, or sends CONNECT to the phone. `UplinkPhoneOnly` never uses the home
  transport or dialer, even when the phone fails or is cooling down, and still
  requires `HomeIPv4` (the ingress peer and bind address). That requirement is
  unchanged and is not relaxed for phone-only use.
- `HomeIPv4 string`; `AllowHomePeer bool` only adds that exact address to allowed peers.
- `PhoneProxyAddr string` (numeric loopback IP:port only; default 127.0.0.1:8283), `PhoneToken string`.
- `HomeWeight, PhoneWeight int`; each must be positive, except both zero use 1:4.
- `AllowedHosts []string`: replaces defaults `steamcontent.com`, `steampipe.akamaized.net`.
  Each normalized DNS name allows itself and dot-boundary subdomains; no IPs/wildcards.
- `MaxConcurrent int` (64), `MaxHeaderBytes int` (32 KiB),
  `HeaderTimeout time.Duration` (15s), `IdleTimeout time.Duration` (30s),
  `DialTimeout time.Duration` (10s), `RequestTimeout time.Duration` (30m),
  `FailureCooldown time.Duration` (5s).
- `MaxPhoneConnections int` (32): optional total phone socket budget; zero uses 32,
  explicit values must be 1..32. Reservations cover dialing (including detached
  transport dials), active transfers and all idle per-host keepalive pools until
  explicit socket close (EOF alone is a half-close). Keepalive remains enabled;
  when full, idle connections are evicted so rotating CDN hosts need not wait for `IdleTimeout`. If still full,
  the request may fall back to home without a phone error/cooldown. This is a
  local socket bound, not an acknowledgement of the proxy's asynchronous FIN
  cleanup. `Close` cancels pending dials and closes active and idle phone sockets.
- `LookupIP func(context.Context, string) ([]net.IP, error)`: trusted resolver seam,
  called **only for the home path**, once per home attempt, after the allowlist.
  The phone path never calls it (the phone resolves the hostname), so a PC DNS
  failure cannot block the phone. Default uses system DNS; **hosts/lancache DNS overrides require an independent
  public-origin resolver here**, otherwise private answers intentionally fail closed.
- `HomeDialContext, PhoneDialContext func(context.Context, string, string) (net.Conn, error)`:
  trusted test/integration seams. Home override must enforce source/interface binding.
- `HomeTransport, PhoneTransport http.RoundTripper`: trusted test seams, replacing
  production transports; URL/Host/DNS checks still run, but seams own socket safety.

Security/behavior contract: only loopback peers (and HomeIPv4 if explicitly enabled).
HTTP ingress allows GET/HEAD, strict allowlisted DNS Host, origin ports 80/443;
origin port 443 uses verified TLS with the original hostname, not plaintext.
Each path resolves independently. Home: the PC resolves per attempt and all answers
must be public (a mixed answer is rejected without dialling); sockets are pinned to
numeric addresses, retaining original HTTP Host (no second DNS lookup). At most 4
distinct IPv4 answers are tried, each with `max(DialTimeout/min(n,4), 1s)`, and the
whole dial is capped by `DialTimeout`, so a blackholed first address cannot starve the
next. Phone: `CONNECT host:port` with the allowlisted hostname; the phone resolves it
and validates the actual connected peer. A phone CONNECT `403` means the origin was
rejected: it sets no cooldown and counts no error, and the request tries home. A
home resolve rejection (non-public, empty or over-64 answers) is released as "not an
attempt" (no Request, no error, no cooldown) and tries the phone; a home DNS error
is a home failure with cooldown. If no path serves the request the client gets 403
when any path rejected the origin, otherwise 502 (503 while paths cool down or the
phone is stale; phone-only never falls back to home). CONNECT uses bearer auth,
validates HTTP 200, bounds headers, and preserves buffered tunnel bytes. No proxy
auth is forwarded to origins. Hop-by-hop headers and Connection-nominated headers
are removed in both directions. Concurrency, headers, streaming buffers and timeouts
are bounded. Retry the other uplink only before a response starts; never retry a
partial response. No CONNECT ingress or TLS MITM. See opaque TLS ingress below.

Compatibility limits: install/hosts/DNS, Steam cache discovery/adoption, Windows physical
routing, actual throughput gains and iPhone cellular behavior are NOT proven by
this package. In particular modern Steam's adoption of local HTTP caches is uncertain;
this does not redirect arbitrary Steam HTTPS traffic. Phone must support CONNECT to
public IPv4/IPv6 port 80 (and 443 if used by a trusted caller); CONNECT443-only is
insufficient for Steam HTTP downloads. The launcher owns bind address and firewall.

Windows socket option reference: [Microsoft IPPROTO_IP socket options](https://learn.microsoft.com/en-us/windows/win32/winsock/ipproto-ip-socket-options) (`IP_UNICAST_IF`, network-byte-order interface index).

## Phone token refresh

`SetPhoneToken(token string) error` replaces the phone bearer token on a running relay;
`NormalizePhoneToken(raw string) (string, error)` is the canonicalising validator it uses.

Formats. `NormalizePhoneToken` removes spaces, tabs, CR/LF and hyphens (the on-screen
grouping of 4), lowercases ASCII letters, and then requires exactly one of:
- 32 characters of lowercase Crockford base32 (`0123456789abcdefghjkmnpqrstvwxyz`:
  no `i l o u`), the iOS app's new format (20 random bytes); or
- 64 lowercase hex characters, the legacy iOS token (still accepted).
Crockford's decoding aliases (`O` to `0`, `I`/`L` to `1`) are deliberately NOT applied: the
bearer string must equal what the phone holds. Errors never contain the input.
`Config.PhoneToken` keeps its older, looser check (printable ASCII, no whitespace, at most
4096 bytes) so existing callers and tests still work; only `SetPhoneToken` is strict.
`SetPhoneToken` returns an error in `UplinkHomeOnly` (no phone path) and on a closed relay.

State machine (`Status.PhoneAuth`: `Stale`, `Generation`, `Rejections`, `StaleSince`; never the token):
- The token and its generation are one pair, read and written under one lock. `Config.PhoneToken`
  is generation 1 (0 if none was given); each accepted `SetPhoneToken` adds 1.
- A CONNECT answered `407` marks the phone `Stale` for the generation that was sent. It is NOT a
  path error: no `Errors++` and no cooldown (`errPhoneAuth` joins the local-capacity class). A 407
  from an older generation is ignored, so it cannot invalidate a fresher token. `Rejections` counts
  407s for the current generation; `StaleSince` is when `Stale` was first set.
- While `Stale`, the scheduler (`acquire`) does not choose the phone, so the stale token is not
  sent again. A CONNECT already in flight may still send it once.
- `UplinkDual` continues on home in the same request (HTTP and TLS). `UplinkPhoneOnly` fails
  closed: 503 for HTTP and a closed session for TLS, and the home path is never used.
- `SetPhoneToken` installs the new token and generation, clears `Stale`, and closes IDLE phone
  sockets (keep-alive tunnels opened with the old token). Active transfers are not interrupted.
  An invalid token changes nothing, including `Stale`.
- `SetPhoneToken` does not contact the phone. A caller that must verify a token before swapping
  it (the CLI's authenticated diagnostic probe) does that first.

## Opaque TLS ingress (stable API)

`ServeTLS(net.Listener) error` owns and closes the supplied listener. The caller
binds the exact local `HomeIPv4:443` (never wildcard); this package does not bind
ports, install certificates or edit DNS. Like HTTP, only `peerAllowed` peers are
admitted. HTTP and TLS share `MaxConcurrent`, uplink scheduling, cooldowns and
`MaxPhoneConnections`. Raw TLS uses the configured dial seams, not RoundTrippers.

ClientHello inspection is read-only: at most 64 KiB captured wire bytes, bounded
by `HeaderTimeout`. Fragmented records and every captured byte are preserved.
Only strict allowlisted visible DNS SNI is accepted. The home path resolves per attempt
(all results must be public) and pins numeric port 443; the phone gets `CONNECT host:443`.
A rejected origin closes the session without bytes or alerts when no path serves it. Missing/invalid SNI and ECH offers
are rejected (inner SNI is unavailable); no TLS records or alerts are generated.
The origin and client perform end-to-end TLS and normal certificate validation.
Home uses the same source/interface-bound dialer; phone uses authenticated CONNECT443 by hostname.

Fallback is allowed only on dial/CONNECT failure or a ClientHello write failure
that wrote zero bytes. Once any TLS byte is sent upstream, never retry/splice.
Two 32 KiB copy buffers bound/backpressure each session. `IdleTimeout` bounds
lack of progress across both directions; `RequestTimeout` bounds the entire session. Clean EOF
half-closes the opposite write side when supported; otherwise both sides close
(safe truncation, never replay). Other I/O failures close both sides.
`Relay.Close` and listener termination cancel/close that listener's sessions and
wait for their exit; no HTTP Shutdown call is required. TLS status Bytes counts
encrypted downstream wire bytes updated during active streams (not only at close); HTTP Bytes remains response-body bytes.

Path health: the first cause of a spliced TLS session ends is recorded before any
socket closes. Upstream read/write errors, upstream EOF with zero downstream bytes,
and an idle expiry whose last progress was client-to-upstream (upload stall) count
as path failures (error + `FailureCooldown`). Idle after downstream progress
(keep-alive), client errors/resets and cancellation never do.

HTTP: each attempt has its own cancellable context. While blocked reading the origin
body, `IdleTimeout` of no bytes cancels the attempt: a path failure with cooldown,
and the committed response is aborted (never retried or spliced). Every downstream
write gets an `IdleTimeout` write deadline (via `http.ResponseController`, ignored
if unsupported); a stalled client ends the response but is not a path failure.

This enables only explicitly redirected allowlisted local-cache TLS traffic. It
does not prove current Steam cache adoption, iPhone routing or speed improvements,
and does not intercept arbitrary TLS. DNS overrides still require an independent
public-origin resolver.
