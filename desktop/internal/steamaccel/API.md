# Steam HTTP dual-uplink relay API (Go 1.22)

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
// r also implements http.Handler (GET/HEAD only).
// r.Snapshot() returns Status{Home, Phone PathStatus}.
```

Stable public surface: `New(Config) (*Relay, error)`, `(*Relay).ServeHTTP`,
`(*Relay).Server(addr string) *http.Server`, `(*Relay).Snapshot() Status`,
`(*Relay).Close() error`. `PathStatus` exposes `Requests`, `Active`, `Bytes`,
`Errors` (uint64), `CooldownUntil` (time.Time); `Status` exposes `Home`, `Phone`.
Bytes mean successfully written downstream response-body bytes, not wire bytes.
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
- `LookupIP func(context.Context, string) ([]net.IP, error)`: trusted resolver seam.
  Default uses system DNS; **hosts/lancache DNS overrides require an independent
  public-origin resolver here**, otherwise private answers intentionally fail closed.
- `HomeDialContext, PhoneDialContext func(context.Context, string, string) (net.Conn, error)`:
  trusted test/integration seams. Home override must enforce source/interface binding.
- `HomeTransport, PhoneTransport http.RoundTripper`: trusted test seams, replacing
  production transports; URL/Host/DNS checks still run, but seams own socket safety.

Security/behavior contract: only loopback peers (and HomeIPv4 if explicitly enabled),
GET/HEAD, strict allowlisted DNS Host, origin ports 80/443. Ingress is HTTP;
origin port 443 uses verified TLS with the original hostname, not plaintext. All DNS answers must be
public; origin sockets/CONNECT targets are pinned to validated numeric addresses,
retaining original HTTP Host (no second DNS lookup). CONNECT uses bearer auth,
validates HTTP 200, bounds headers, and preserves buffered tunnel bytes. No proxy
auth is forwarded to origins. Hop-by-hop headers and Connection-nominated headers
are removed in both directions. Concurrency, headers, streaming buffers and timeouts
are bounded. Retry the other uplink only before a response starts; never retry a
partial response. No CONNECT listener, HTTPS interception, SNI passthrough or MITM.

HTTP-only MVP: install/hosts/DNS, Steam cache discovery/adoption, Windows physical
routing, actual throughput gains and iPhone cellular behavior are NOT proven by
this package. In particular modern Steam's adoption of local HTTP caches is uncertain;
this does not redirect arbitrary Steam HTTPS traffic. Phone must support CONNECT to
public IPv4/IPv6 port 80 (and 443 if used by a trusted caller); CONNECT443-only is
insufficient for Steam HTTP downloads. The launcher owns bind address and firewall.

Windows socket option reference: [Microsoft IPPROTO_IP socket options](https://learn.microsoft.com/en-us/windows/win32/winsock/ipproto-ip-socket-options) (`IP_UNICAST_IF`, network-byte-order interface index).
