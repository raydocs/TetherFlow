# usbmux API (stable integration contract)

Standard-library-only client of the **existing Apple USBmux service**, not a USB driver.
Windows needs Apple Devices / its USB stack; no separately installed `iproxy` is used.
Default service: Windows `tcp`, `127.0.0.1:27015`; other platforms `unix`,
`/var/run/usbmuxd`. TCP endpoints must be numeric loopback; Unix paths absolute.

```go
client, err := usbmux.NewClient(usbmux.Config{})
devices, err := client.ListUSB(ctx)
device, err := usbmux.SelectUSB(devices, deviceID, serial) // 0 / "" = unspecified
listener, err := net.Listen("tcp", "127.0.0.1:0") // caller binds, numeric loopback only
forwarder, err := usbmux.New(client, device)
go func() { err := forwarder.Serve(ctx, listener); _ = err }()
// use listener.Addr(); caller handles bearer authentication unchanged
// later: forwarder.Close()
```

- `Config{Network, Address string; HandshakeTimeout time.Duration;
  DialContext func(context.Context, string, string) (net.Conn, error)}`.
  Zero timeout = 10 seconds. Negative timeout is invalid. Dial seam must honor context
  and return the specified local service, not another device/service.
- `Device{DeviceID uint32; Serial, ConnectionType string; ConnectionSpeed uint64}`.
  Only `ConnectionType == "USB"` is returned/accepted. Serial is normalized by
  trimming space, removing hyphens, and lowercasing (UDID/SerialNumber aliases must
  agree). Speed is service-reported metadata, **not a throughput guarantee**.
- `SelectUSB([]Device, uint32, string) (Device, error)` matches both supplied filters.
  Multiple matches return `ErrSelectionRequired`; no match returns `ErrDeviceNotFound`.
  No automatic Wi-Fi sync, network-device selection, or disconnected-device fallback.
- `Client.ListUSB(context.Context) ([]Device, error)` and
  `Client.Connect(context.Context, Device, uint16) (net.Conn, error)` are concurrency-safe.
  Connect rechecks the exact USB ID and normalized serial on its control connection,
  then sends Connect. A zero port is invalid. The returned stream preserves bytes
  arriving with the Result response. Context/deadline cover the entire handshake;
  after success the raw stream deadline is cleared and the caller owns its lifetime.
- `New(*Client, Device) (*Forwarder, error)` pins the selected USB device and port
  **8283**. `Forwarder.Serve(context.Context, net.Listener) error` runs once and owns
  the listener after entry (including validation failure); non-loopback peers are skipped.
  At most 32 accepted/dialing/relaying sessions; each direction uses a bounded 32 KiB
  buffer with TCP/Unix half-close and backpressure. Idle means no successful read/write
  progress in either direction for 30 seconds; maximum session lifetime is 30 minutes
  from accept, including handshake. Serve waits for all sessions on exit.
- `Forwarder.Close() error` is idempotent, cancels accept/dials/active streams. It can
  precede Serve. Close does not itself wait; Serve returning proves teardown.
  Serve returns `ErrClosed` if already closed before entry, nil after Close during Serve,
  context error after parent cancellation,
  and the accept/validation error otherwise. Session failures close that socket only.
- Protocol/malformed input wraps `ErrProtocol`; Result nonzero is a `*ResultError`
  exposing `Number uint64`. Selection errors are usable with `errors.Is`.

XML accepts the canonical inert Apple plist PUBLIC DOCTYPE but never fetches it.
Internal subsets/entities, unknown directives, duplicate keys and excessive sizes,
node counts or nesting are rejected. Packets including header are at most 1 MiB.
Protocol facts independently implemented from upstream references (no copied code):
[libusbmuxd.c](https://github.com/libimobiledevice/libusbmuxd/blob/master/src/libusbmuxd.c),
[client.c](https://github.com/libimobiledevice/usbmuxd/blob/master/src/client.c).

Actual Apple Devices/macOS service compatibility, physical USB (including USB 2/3),
and device throughput remain unverified. This is not a replacement driver and makes
no 1.2 Gbit/s claim. Root owns CLI wiring and end-user setup guidance.
