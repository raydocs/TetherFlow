# Verification evidence

Executed on Darwin arm64, Go 1.27.3 (2026-10-24), scoped to this package:

| Command | Result |
| --- | --- |
| `go test -race -count=3 ./internal/usbmux` | PASS, 3.948s |
| `go vet ./internal/usbmux` | PASS |
| `GOOS=windows GOARCH=amd64 go test -c -o /tmp/tetherflow-usbmux.test.exe ./internal/usbmux` | PASS (PE32+ x86-64 executable) |
| `GOOS=windows GOARCH=arm64 go test -c -o /tmp/tetherflow-usbmux-arm64.test.exe ./internal/usbmux` | PASS (cross-compile only) |
| `go test -cover ./internal/usbmux` | PASS, 85.7% statement coverage |

Tests use actual loopback TCP sockets, not a mocked packet transport. Covered:
canonical libplist XML/Apple DOCTYPE, byte-fragmented packets, oversize/truncated
frames, bad header version/message/tag, malformed/bounded XML, duplicate keys,
entity/directive rejection, inconsistent device IDs and serial aliases, network
exclusion, selection ambiguity, exact ID+serial revalidation, nonzero Result,
dial/list/connect cancellation, handshake deadline and deadline clearing,
coalesced Result/raw bytes plus EOF, four concurrent 2 MiB exact-byte round trips
with trailing bytes and half-close, 32-session budgets during both dialing and
active relay, backpressured shutdown, cancellation/teardown, shared progress idle
refresh and maximum lifetime, non-loopback listener/peer rejection, and no device
fallback after disconnect. Tests use shortened private timer values to exercise
lifecycle paths; production values remain 30 seconds idle / 30 minutes lifetime.

No CLI/authentication/driver changes. No commits, pushes, or delegated tasks.
New files are confined to `desktop/internal/usbmux/`; cross-compile outputs are
outside the repository. Other existing worktree edits were not modified.

Not verified: Windows runtime execution; an actual Apple Devices/macOS USBmux
service; physical USB 2/3 hardware, phone-app interoperability, or throughput.
No 1.2 Gbit/s claim. This requires the existing Apple USB stack and is not a USB
driver or an installer. Root owns CLI wiring and actual-device acceptance.
