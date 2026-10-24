# TetherFlow foreground iOS USB proxy prototype

This is a new, standalone experiment, not a port of the Android engine. It does
not change the existing desktop application or its `8282 /pac` and ADB probes.
No physical iPhone, Windows, Steam, cellular throughput, USB link rate, or
Wi-Fi + cellular aggregation result has been established. See [VALIDATION.md](VALIDATION.md).

## Build and host tests (Mac)

Prerequisites: installed Xcode/command-line tools, XcodeGen (tested 2.46.0), Swift,
Python 3. No Swift package dependencies, simulator, emulator, downloaded runtime,
Network Extension entitlement, VPN configuration, or TLS certificate installation.

```sh
cd ios
./tools/validate.sh
```

This runs macOS-host XCTest and Python tests, generates the Xcode project from
`project.yml`, and requires an unsigned generic-device scheme build. Local Xcode
26.6/SDK 26.5 has intermittent destination-discovery failures: a genuine generic
build success is recorded, but the latest recheck failed. `build-device.sh` does
not hide that failure behind a target-only fallback or install components. See
[VALIDATION.md](VALIDATION.md) for exact evidence and deployment limitations.

Generated project/build directories are ignored by `ios/.gitignore`. Re-generate:

```sh
xcodegen generate --spec project.yml
open TetherFlowIOS.xcodeproj
```

For a physical device, select your own signing Team and a unique bundle identifier
in Xcode, enable Developer Mode, trust/pair the USB-connected phone, then build
and run a signed app. The unsigned `.app` is compilation evidence, not an
installable release. Xcode 26.6/SDK 26.5 does **not** establish iOS 27 device support;
you may need a compatible Apple Xcode/device-support installation on your build
machine before a signed iOS 27 deployment. This task did not install one.

## USB setup (physical evidence still required)

The new Windows Steam launcher has a built-in USBmux client and does **not**
require a separate iproxy install by default; follow
[Windows setup](../desktop/steam-tools/README.zh-CN.md). The manual iproxy steps
below remain an optional standalone diagnostic path, especially on macOS.


1. Supply working Apple mobile-device USB drivers/services and pairing on the
   host. macOS and Windows installation/distribution requirements differ.
2. Supply `iproxy` from libusbmuxd, a paired/trusted device, and a data-capable cable.
   `iproxy` was **not installed here**. USB forwarding is external to this app.
3. Open the signed phone app, enable cellular data for it, keep it active/unlocked,
   tap **Start**, and securely transfer the displayed session token to the host.
   Do not paste secrets into chat or public logs. Copy is device-local and expires
   after two minutes; the token itself remains valid until Stop/foreground loss.
4. With current libusbmuxd syntax, explicitly use USB and host loopback:

   ```sh
   iproxy -l -s 127.0.0.1 -u YOUR_DEVICE_UDID 8283:8283
   ```

   Check your installed `iproxy --help`; older versions use positional
   `8283 8283` instead of `8283:8283`. Never use `-n` (network device discovery)
   or bind `0.0.0.0` for this prototype. If loopback forwarding fails on the
   actual iOS version, collect errors; do not silently widen the phone listener.
5. Run the CLI on the USB host; it prompts privately for the token (or accepts
   `TF_TOKEN` from a private environment, never a command-line token argument):

   ```sh
   python3 tools/smoke.py download --bytes 33554432
   python3 tools/smoke.py connect --url https://example.com/
   python3 tools/smoke.py connect --url http://example.com/
   ```

   The last two tests use CONNECT to 443 and 80 respectively. HTTPS validates the
   origin certificate with the host's normal TLS trust store. HTTP :80 is plaintext.
   The smoke only reads a capped response sample, not an Internet benchmark.

The download diagnostic returns deterministic **locally generated** bytes. Its
JSON `verified_payload_mbps` measures delivered phone→host payload including Python
verification overhead. It does not measure cellular, negotiated USB speed, or
Steam performance. Use several sizes/repetitions, record device/OS/cable/port,
thermal state and errors, and distinguish transport versus Internet tests.
A phone-local diagnostic cannot prove cellular egress works.

## Wire protocol (for Windows relay integration)

Phone endpoint: **127.0.0.1:8283**, reached through USB port forwarding to host
**127.0.0.1:8283**. Every socket carries exactly one authenticated request:

```http
CONNECT cdn.example.com:80 HTTP/1.1
Host: cdn.example.com:80
Proxy-Authorization: Bearer <current-session-token>

```

Use CRLF, including the final empty line. Only destination ports **80 and 443**
are accepted; IPv4/DNS authorities and bracketed IPv6 authorities are supported.
A successful response is exactly `HTTP/1.1 200 Connection Established\r\n\r\n`,
then an opaque TCP byte relay. CONNECT headers (including the token) are consumed
by the phone and not forwarded to the origin. Bytes already following the
request header are preserved. The relay preserves TCP half-close in both
directions; it does not parse the HTTP CDN traffic or intercept TLS.

Diagnostic request: `GET /diagnostic/download?bytes=N HTTP/1.1` with `Host` and
identical `Proxy-Authorization`; N is 1...268435456. Response has Content-Length
and byte `offset % 251` throughout its body. No upload/body, chunked request,
pipelining, general forward-HTTP GET, `/pac`, UDP/QUIC, SOCKS, or persistent
proxy-request parsing is implemented. HTTP inside a CONNECT :80 tunnel is allowed.
HTTP/1.1, ASCII headers, and one nonempty Host header are required. Duplicate
headers, folding/tabs/control bytes, request bodies and ambiguous framing are
rejected. This is a deliberately narrow HTTP subset.

Responses: 407 missing/wrong token (Bearer challenge), 403 disallowed destination
port, 431 header >16 KiB including CRLF terminator, 400 malformed/bad diagnostic
size, 405 unsupported route, 408 header deadline, 502 rejected/failed egress,
504 connection deadline. All error bodies are empty and connections close.
The 33rd simultaneous accepted socket is closed rather than queued indefinitely.

The Windows relay must explicitly implement this Bearer header; standard system
proxy settings or Basic proxy credentials alone do not provide it. This phone
app does not install/configure Windows interception or Steam lancache DNS.

## Safety, resource bounds, and lifecycle

- Listener binds IPv4 loopback only via `requiredLocalEndpoint`; no wildcard,
  Bonjour or unauthenticated Wi-Fi proxy. The host `iproxy` must also remain local.
- 32 bytes from `SecRandomCopyBytes`, encoded as 64 hex characters, form a fresh
  in-memory token on **each** Start. No persistence or token/payload logging.
  All routes require exact Bearer authentication. Local/pairing trust is still
  required: this is not a hardened multi-user service or an encrypted USB protocol.
- Outbound `NWConnection` uses `requiredInterfaceType = .cellular` and disabled
  multipath. No default-interface retry or Wi-Fi fallback is implemented. Before
  200, the ready path must be satisfied and cellular; subsequent non-cellular or
  unsatisfied paths close the tunnel. Waiting/unavailable cellular times out.
  DNS is delegated to Network.framework/system resolution; this prototype makes
  **no independent DNS-privacy/interface-routing guarantee**.
- One serial queue owns sockets, session table, deadlines and cancellation.
  A maximum of **32 sessions total** includes diagnostic and incomplete requests.
  Receives are <=64 KiB, and each direction waits for send `contentProcessed`
  before receiving another chunk. The diagnostic reuses a 64,256-byte pattern;
  it never allocates N bytes. Application in-flight payload is O(32 × 64 KiB per
  direction), plus bounded header/initial-data buffers and framework/kernel buffers.
  This is a bound on application queuing, not a measured total-RSS guarantee.
- Header completion: absolute 10 s (slow trickles do not extend it); upstream
  connection: 15 s; active transfer inactivity: 30 s; diagnostic total: 60 s;
  error-response close: 5 s. A continuously progressing CONNECT has no total
  lifetime cap. Timeouts are checked once a second on the serial queue.
- Stop/scene **inactive or background**, including lock or control-center
  transitions, cancels listener, sessions, receives, sends, and timers. No automatic
  restart; tap Start again and use the new token. Auto-lock is disabled only while
  running, restored on stop. There is no background mode or keepalive workaround.
- Session logs report payload counters/duration/reason, not achieved bandwidth.
  The screen retains the last 100 log lines only. It does not export analytics.

## Explicit non-capabilities

Not a VPN, system-wide tether, transparent Windows proxy, bandwidth bond,
MPTCP implementation, PAC/ADB compatibility layer, carrier-billing bypass, or
production Steam accelerator. Windows may eventually distribute independent
CDN requests between home Wi-Fi and the cellular relay, but that is outside
this app and has not been demonstrated here. A 10 Gbps cable and a phone speed
of 1200 Mbps do not establish this path's throughput. Carrier policy/data limits,
cellular signal, device heat, forwarding overhead, and connection scheduling apply.
Physical iOS 27 and Windows integration evidence remains a release gate.

## Primary references

- [Apple NWParameters](https://developer.apple.com/documentation/network/nwparameters)
  and [requiredLocalEndpoint](https://developer.apple.com/documentation/network/nwparameters/requiredlocalendpoint)
  document the interface and binding controls used here.
- [libusbmuxd iproxy manual](https://github.com/libimobiledevice/libusbmuxd/blob/master/docs/iproxy.1)
  documents USB selection, source address, and current port syntax. Loopback
  reachability of this particular app on the target phone is still unverified.
