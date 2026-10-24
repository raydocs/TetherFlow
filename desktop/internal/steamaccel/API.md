# Steam HTTP and opaque TLS dual-uplink relay API (Go 1.22)

```go
r, err := steamaccel.New(steamaccel.Config{
    HomeIPv4: "192.168.1.10", // REQUIRED: assigned Wi-Fi IPv4; home sockets bind it
    AllowHomePeer: true,       // permit this exact source IP as well as loopback
    PhoneProxyAddr: "127.0.0.1:8283", // default
    PhoneToken: token,         // REQUIRED; sent only to local phone CONNECT proxy
    HomeWeight: 1, PhoneWeight: 4, // zero/zero selects these defaults
})
if err != nil { /* handle */ }
defer r.Close()
s := r.Server("192.168.1.10:80") // bounded HTTP server; call ListenAndServe yourself
// Optional opaque TLS ingress: caller binds exact HomeIPv4:443, then r.ServeTLS(listener).
// r also implements http.Handler (GET/HEAD only).
// r.Snapshot() returns Status{Home, Phone PathStatus}.
```

Stable public surface: `New(Config) (*Relay, error)`, `(*Relay).ServeHTTP`,
`(*Relay).Server(addr string) *http.Server`, `(*Relay).ServeTLS(net.Listener) error`,
`(*Relay).Snapshot() Status`,
`(*Relay).Close() error`. `PathStatus` exposes `Requests`, `Active`, `Bytes`,
`Errors` (uint64), `CooldownUntil` (time.Time); `Status` exposes `Home`, `Phone`.
For HTTP, Bytes mean successfully written downstream response-body bytes.
For opaque TLS, Bytes mean encrypted downstream wire bytes.
Requests count outbound attempts. A canceled inbound request does not count as a
path error. Weights are weighted least-outstanding, not bandwidth bonding.

Config (all optional except HomeIPv4 and PhoneToken):
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
- `LookupIP func(context.Context, string) ([]net.IP, error)`: trusted resolver seam.
  Default uses system DNS; **hosts/lancache DNS overrides require an independent
  public-origin resolver here**, otherwise private answers intentionally fail closed.
- `HomeDialContext, PhoneDialContext func(context.Context, string, string) (net.Conn, error)`:
  trusted test/integration seams. Home override must enforce source/interface binding.
- `HomeTransport, PhoneTransport http.RoundTripper`: trusted test seams, replacing
  production transports; URL/Host/DNS checks still run, but seams own socket safety.

Security/behavior contract: only loopback peers (and HomeIPv4 if explicitly enabled).
HTTP ingress allows GET/HEAD, strict allowlisted DNS Host, origin ports 80/443;
origin port 443 uses verified TLS with the original hostname, not plaintext. All DNS answers must be
public; origin sockets/CONNECT targets are pinned to validated numeric addresses,
retaining original HTTP Host (no second DNS lookup). CONNECT uses bearer auth,
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

## Opaque TLS ingress (stable API)

`ServeTLS(net.Listener) error` owns and closes the supplied listener. The caller
binds the exact local `HomeIPv4:443` (never wildcard); this package does not bind
ports, install certificates or edit DNS. Like HTTP, only `peerAllowed` peers are
admitted. HTTP and TLS share `MaxConcurrent`, uplink scheduling, cooldowns and
`MaxPhoneConnections`. Raw TLS uses the configured dial seams, not RoundTrippers.

ClientHello inspection is read-only: at most 64 KiB captured wire bytes, bounded
by `HeaderTimeout`. Fragmented records and every captured byte are preserved.
Only strict allowlisted visible DNS SNI is accepted, all DNS results must be public,
and destinations are numeric pinned port 443. Missing/invalid SNI and ECH offers
are rejected (inner SNI is unavailable); no TLS records or alerts are generated.
The origin and client perform end-to-end TLS and normal certificate validation.
Home uses the same source/interface-bound dialer; phone uses authenticated CONNECT443.

Fallback is allowed only on dial/CONNECT failure or a ClientHello write failure
that wrote zero bytes. Once any TLS byte is sent upstream, never retry/splice.
Two 32 KiB copy buffers bound/backpressure each session. `IdleTimeout` bounds
lack of progress across both directions; `RequestTimeout` bounds the entire session. Clean EOF
half-closes the opposite write side when supported; otherwise both sides close
(safe truncation, never replay). Other I/O failures close both sides.
`Relay.Close` and listener termination cancel/close that listener's sessions and
wait for their exit; no HTTP Shutdown call is required. TLS status Bytes counts
encrypted downstream wire bytes updated during active streams (not only at close); HTTP Bytes remains response-body bytes. TLS
stream errors after forwarding do not diagnose/poison uplink health.

This enables only explicitly redirected allowlisted local-cache TLS traffic. It
does not prove current Steam cache adoption, iPhone routing or speed improvements,
and does not intercept arbitrary TLS. DNS overrides still require an independent
public-origin resolver.
