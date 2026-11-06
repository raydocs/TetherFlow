# Go ↔ production Swift protocol integration

**Current status (2026-11-06).** Independent review R1 is **block** and the latest CI run is
**pending**; real-hardware gates H1–H7 are **unverified**. The cross-language runs below are host
fixtures (no device, Apple Devices, carrier or Steam), and nothing here is a 1200 Mbps, speed or
Steam-adoption claim. The dated evidence that follows is history and is kept unchanged.

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

## 2026-11-06 — reliability rework addendum

- The fixture's origin and the Go relay now cover the reworked token generations, per-path DNS (the phone
  resolves a `CONNECT host:port` itself; fixtures use a hostname, which is a protocol change from IP
  literals, with the integrity and byte-budget assertions kept) and the shared
  `integration/public-ip-vectors.tsv` that both the Go and Swift suites read.
- Main-session records: `TF_INTEGRATION_COUNT=3` after the relay-health change, `10` after DNS (debug and
  optimized Swift fixture builds, direct and native-USBmux modes), and `10` low-concurrency rounds after
  adding an `origin.Write` error and short-write assertion; none ignored a failure. Raw logs are local
  `/tmp/tetherflow-r9-integration.log` style files and are not committed.
- **Unresolved risk, not closed:** the review-R1 run saw 9 unexpected EOFs and a count mismatch that passed
  on rerun; the root cause of that run is **unknown**. An investigation (no production change) reproduced,
  only under extreme load, the origin itself failing with `ENOBUFS`/`EPIPE` alongside EOFs. That is evidence
  of one load-induced failure mode and does **not** prove it is the same cause as the review failure. Do not
  count a rerun as fixing it; CI must keep failing on truncations.
- Windows health classification (client already finished plus some downstream bytes, then an upstream error
  counts as teardown) was changed after a Windows CI failure. It is a TLS-opaque heuristic, not a proof the
  whole response arrived, and the Windows CI result for it is pending. Host fixtures here do not execute the
  Windows E2E (R14), which has not yet run.
