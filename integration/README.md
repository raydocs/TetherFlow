# Go ↔ production Swift protocol integration

On macOS run `./integration/verify-host.sh`; optionally set
`TF_INTEGRATION_COUNT=20` for repeated runs. This builds the unmodified production
Swift parser/server alongside a **host-only** fixture that injects a local origin,
then sends concurrent transfers through Go's real home/phone transports, in both
direct-loopback mode and through the new production Go USBmux client/forwarder
against a protocol-service fixture. It also tests real, certificate-verified TLS
through both uplinks without decrypting at the relay. No
simulator, device, carrier network or Internet origin is used.

The fixture source is outside the iOS app/project and Swift package. Its injected
non-cellular egress is never included in a production app. Its command-line token
is a fixed TEST-ONLY value, not a real phone credential.

The test verifies 20 concurrent plus10 sequential random 2 MiB+137-byte responses end-to-end, persistent tunnel reuse, both
paths' actual body counters, absence of origin credential leaks, and failover after
terminating the Swift process. It does not turn a host transport result into a USB
or cellular Mbps claim.

Evidence (2026-10-24): the first new integration invocation had unexpected EOF on
7 transfers (before fixture log instrumentation); the other two repetitions passed.
After adding fixture lifecycle log output only, 3/3 and then20/20 repetitions passed.
No parser/relay behavior, expected byte counts or error assertions were changed to
make the test pass. The initial failure's root cause is not established. Preserve
this as residual reliability risk; CI repeats the integration test10 times and must
not ignore future truncations. Local raw logs are retained in `/tmp/tetherflow-crosslanguage*.log`.

Latest post-fix evidence: production Swift buffered-state lifetime defect was
reproduced and repaired (see `ios/VALIDATION.md`). Ten full integration iterations
with two modes each passed, including HTTP payload equality, persistent chunks,
end-to-end certificate-verified opaque TLS and phone-disconnect fallback. Native
USBmux uses a simulated local protocol service, NOT Apple Devices or USB hardware.
The first native fixture compile caught an overflowing typed-constant port swap;
masking the lower byte before shifting corrected the fixture (not production).
