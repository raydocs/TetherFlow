# Steam reliability: contracts and execution plan

Date: 2026-11-06. Inspected at `9c6eb78`; `wip/steam-ios-dual-download` is at `dee2411`, which differs
only in `docs/PROGRESS.md`. Author: design sub-agent (Opus 5.5). This document changes no source.

Success for this plan: every Astra finding below maps to a contract and to named tasks whose tests can
fail on today's code, and everything that automation cannot prove is listed as a hardware gate.

**Never claimed by this work:** 1200 Mbps, any speed gain, or a fix for Apple's iPhone Ethernet driver.
USBmux needs no iPhone Ethernet adapter, but it still depends on the Apple USB service (Apple Devices).
Earlier review R2 approved the old contract and does not carry over to anything here.

Astra's nine-unit list was not available to me (not in the repo, and a thread search found nothing), so
the units below are derived from the seven findings. Check them against Astra's list if it matters.

## 1. Findings confirmed in source

| # | Finding | Anchor | Task(s) |
|---|---|---|---|
| F1 | Forwarder pins the old DeviceID, so the same phone with a new ID never reconnects | `usbmux/client.go:340-345`, `forwarder.go:148` | R1 |
| F2 | iOS rotates the token on any non-active scene; the desktop reads it once; the launcher keeps it in a plaintext temp file for the whole run | `TetherFlowApp.swift:20,104`, `main.go:87-99`, `Start-SteamAccelerator.ps1:45-56` | R7, R8, R10 |
| F3 | TLS always ends with `finish(false)`; an HTTP body has no idle deadline | `tls.go:242-245`, `relay.go:324` | R3 |
| F4 | One DNS lookup precedes path choice; IP candidates share one dial budget; the phone accepts any destination | `relay.go:196-214,365,372`, `tls.go:210-222`, `ProxyServer.swift:268` | R4, R9 |
| F5 | HTTP bytes post only at finish; `both_paths_observed` is historical; no single-path modes | `relay.go:327`, `main.go:235` | R6, R11 |
| F6 | Doctor lists interfaces only; `--list-phones` prints an ID that cannot be selected; preflight proves local bytes, not cellular | `main.go:293-313`, `usb.go:27`, `phonecheck.go` | R12 |
| F7 | In-place hosts write has a crash window; guardian makes one attempt and fails silently | `hosts.go:126-141`, `platform_windows.go:82-98` | R2, R5 |

Evidence gathered for this plan (macOS host only, not a device):

- Baseline at `9c6eb78`: `go test` on the four packages passes, `GOOS=windows go vet` passes, and
  `swift test -c release -Xswiftc -enable-testing` passes 19/19.
- Scratch probe: an `NWConnection` dialled by hostname reports a numeric
  `currentPath.remoteEndpoint` once ready. C3 relies on this; **H3 must confirm it on a device.**
- Scratch probe: cancelling an `NWListener` and rebinding the same port at once failed **16/50** with
  `EADDRINUSE`. Auto-resume would hit this, so R8 must fix it.

## 2. Contracts

### C1. Same-serial re-enumeration

The Forwarder's identity is the **normalized serial chosen at startup**, never the DeviceID.

1. `ConnectSerial` lists devices and sends `Connect` on one control connection, using the DeviceID from
   that same list. No ID is cached across connections.
2. It connects only when exactly one `ConnectionType == "USB"` entry has that exact serial.
   - Another serial, a Network entry with the same serial, or an empty serial: `ErrDeviceNotFound`, and
     no `Connect` is sent.
   - Two USB entries with the same serial: `ErrSelectionRequired`, no `Connect`. This fails closed
     during re-enumeration and is re-evaluated on the next accepted connection.
3. There is no retry loop, no scan for "any phone", and no Wi-Fi sync path. If the phone is absent, the
   accepted socket closes and the relay's normal path failure applies.
4. `Client.Connect` keeps its exact ID-and-serial meaning for callers that want it.

Residual: serial is an identifier, not a credential. Something able to register a spoofed serial with
the Apple service would receive the bearer token. Pairing and physical USB access are the trust boundary.

### C2. Explicit token refresh

**Lifetime on iOS**

- The token lives in process memory only: no Keychain, UserDefaults or file.
- The pasteboard keeps its existing `.localOnly` and 2-minute expiry.
- `.inactive`/`.background` still closes the listener and all sockets, but only **pauses**: the token
  stays in RAM for at most 10 minutes, measured with `clock_gettime(CLOCK_MONOTONIC)`, which keeps
  counting through sleep on Darwin. `DispatchTime.uptimeNanoseconds` does not, so it is forbidden here.
- Returning to `.active` inside the window resumes on the same token.
- Window expiry, Stop, Rotate or process death destroys the token.
- This replaces the old "rotates on every Start" contract.

**Format**

- 20 random bytes as 32 lowercase Crockford base32 characters (160 bits), displayed in groups of 4.
- It still meets the existing 32..128 bound. The desktop strips spaces and hyphens and lowercases input.
- Reason: refresh means typing it by hand into a hidden prompt, because Universal Clipboard does not
  reach Windows. 64 hex characters are not usable that way.

**Ingress on the desktop**

- The token arrives on **stdin only**, in the executable's own hidden prompt. `--phone-token-file` and
  the launcher's temp file are removed, so no plaintext token ever reaches disk.
- Not on argv, not in the environment, not in logs or the report.
- Stdlib only: `SetConsoleMode` through `kernel32`, no `x/term`.

**Refresh state machine**

- A CONNECT 407 marks the phone path `Stale`. This is **not** a path error and sets no cooldown.
- While `Stale`, the phone path is not scheduled, so a stale token is not repeatedly sent.
- A 407 from an older token generation cannot mark a newer one stale.
- A new token is probed first with the authenticated diagnostic. Only success swaps it in, which clears
  `Stale` and closes idle phone sockets. A failed probe keeps the old token and state.

**No Wi-Fi fallback**

- Refresh cannot change `EgressPolicy`. The phone stays `requiredInterfaceType = .cellular`.
- `dual` mode serves from the PC's home uplink while stale. `phone` mode returns 503 and never touches
  the home uplink.

Residual: Go strings cannot be zeroed, and the token crosses loopback and USB in clear, so a local admin
on the PC can read it. This is unchanged from today and accepted for a single-user machine.

### C3. Independent per-path DNS and SSRF pinning

| | Home path | Phone path |
|---|---|---|
| Resolver | PC system resolver (`Config.LookupIP`), per attempt | **The phone**, via `CONNECT host:port` |
| Validation | All answers must pass `publicIP`; a mixed answer rejects without dialling | Phone validates the **actual connected peer** |
| Pin | Numeric, source-bound dial | Peer of the established socket |
| Dial budget | At most 4 IPv4 candidates, each `max(DialTimeout/min(n,4), 1s)`, total capped by `DialTimeout` | Network.framework connection attempts, inside `DialTimeout` |

1. The allowlist runs on the desktop before either path. The phone path never calls `LookupIP`, so a
   home DNS failure cannot block it. Carrier DNS also gets the CDN answer suited to the cellular path.
2. Phone-side check, in order:
   - A literal IP authority that is not public gets 403 **before any `NWConnection` exists**.
   - Legacy numeric hosts (`127.1`, `0x7f.0.0.1`, `2130706433`) are malformed, using the same rule as
     Go's `validDNSName`.
   - At `.ready`, `currentPath.remoteEndpoint` must be a numeric public address, otherwise 403. A nil or
     name-typed endpoint fails closed. **No byte is forwarded and no 200 is sent before this check.**
3. Accepted trade-off: a TCP handshake to a non-public peer can complete before rejection, on ports
   80/443 only and with no payload. In return the check validates the real peer, so there is no gap
   between lookup and connect, and Network.framework handles candidate selection.
   Rejected alternatives: a phone `/resolve` route via `DNSServiceGetAddrInfo` (more code and more device
   risk), and desktop DoH (a new external dependency).
4. Go and Swift share one vector file, `integration/public-ip-vectors.tsv`, so the policies cannot drift.
   The only intended difference: `64:ff9b::/96` with an embedded public IPv4 is allowed on the phone
   (NAT64 on IPv6-only carriers) and stays denied on the home path. A carrier-specific NAT64 prefix looks
   like global IPv6, and its embedded address cannot be checked. That is residual.
5. Failure handling:
   - Phone 403 means origin rejected: no cooldown, try the other path.
   - A home non-public answer is released as "not an attempt", then tries the phone.
   - A home DNS error is a home-path failure with cooldown, then tries the phone.
   - If no path succeeds, the client gets 403 if any path rejected the origin, otherwise 502.
6. A new desktop must not silently run against an old phone app, which would have no peer check. The
   authenticated `GET /diagnostic/info` returns `{"protocol":2}`, and preflight and refresh fail on
   anything else with "update the iOS app".

### C4. Transactional, recoverable hosts changes

Keep **in-place writes under the mandatory Windows byte-range lock**, plus a write-ahead journal.

`ReplaceFileW`/rename was rejected:

- It needs a handle with DELETE sharing, so the lock must be dropped, which opens a lost-update window
  against foreign edits.
- Replacing the hosts file is also more likely to trip endpoint security.
- In-place writing preserves the ACL and file identity by construction.

Sequence, all under the lock:

1. **Recover** any `<hosts>.tetherflow-<lease>.journal` first, for every lease:
   - Current file equals `new`, or starts with `new` (foreign text appended later): applied, delete the journal.
   - Current file equals `old`: not applied, delete the journal.
   - Current file is a torn mix, `new[:k] + old[k:]` for some `k`: finish forward to `new`, delete the journal.
   - Anything else: a foreign edit landed after the crash. **Never overwrite.** Keep the journal and
     return `ErrNeedsManualRecovery` naming its path. An unresolved journal blocks every later Update.
2. Read the file and run `Rewrite`. All current refusals stay. If nothing changes, return.
3. Write the journal to a temp file, sync, atomically rename. A torn temp file is ignored and removed.
4. Write in place, truncate, sync. Delete the journal.
5. On a write error, roll back to `old`. If rollback also fails, keep the journal and return `ErrNeedsManualRecovery`.

Other rules:

- The journal sits next to hosts, so it inherits that directory's admin-only write ACL. It exists only
  during an operation or after a crash. No permanent backup file is created.
- Retry applies only to transient errors. On Windows those are errno 5, 32, 33 and 1224. A `Rewrite`
  refusal or manual-recovery error is never retried.
- Budgets: 5 minutes for the guardian, 15 seconds for main-process cleanup (the guardian then takes
  over). `flushDNS` gets 3 attempts.
- A guardian failure writes the lease and error to `--recovery-log`, because it has no console.
- A stale block is reclaimed automatically only when its IP equals `--home-ip` **and** this process
  already holds that address's port 80, which proves the old owner is dead. Otherwise refuse and print
  the exact `--recover-lease` command.
- Boot tasks remain out of scope. After a power cut the entry stays until the next run or until
  `--recover-lease`, and doctor reports it.

Residual: if a power cut persists sectors out of order, the file is not a recognised torn state, so
recovery stops for manual handling. That is safe but not automatic.

## 3. Execution order

Parallel tasks in a wave touch disjoint files, unless a conflict is noted. The main session merges, so
hunk conflicts between neighbours in `relay.go` and `main.go` are its job.

```
Wave 0  R0  integration branch and CI trigger (main session)
Wave 1  R1 usbmux   R2 hosts lib   R3 relay health   R4 iOS peer policy
Wave 2  R5 guardian(R2)   R6 live bytes+modes(R3)   R7 relay token(R3)   R8 iOS token(R4)
Wave 3  R9 per-path DNS(R4,R6,R7)   R10 CLI token+launcher(R5,R7,R8)   R13 iOS Release/signing(R8)
Wave 4  R11 CLI modes+metrics(R6,R10)
Wave 5  R12 doctor+preflight(R1,R4,R9,R11)
Wave 6  R14 Windows E2E+package(R12)
Wave 7  R15 docs, then independent review
```

Critical path, seven tasks: R3 → R7 → R10 → R11 → R12 → R14 → R15. R2 → R5 and R4 → R8 reach R10 at the
same depth, and R6 → R9 joins at R12. R1 and R2 ship value early and can be verified alone. Nothing here
needs hardware.

**R0 (main session, no worker):**

1. Branch `wip/steam-reliability` from `wip/steam-ios-dual-download` and cherry-pick this document. A new
   branch is needed because the main checkout holds the wip branch and has the user's uncommitted Android diff.
2. Add the branch to `on.push.branches` in `.github/workflows/steam-accelerator.yml`.
3. Push, and keep pushing once per wave. CI is one background wait per push, no polling.

**Common block. Paste it with exactly one task section into each `worker-sonnet` prompt.** Never hand
over this whole file as a shared brief.

> Work in your own worktree based on the integration branch tip. One issue, one commit, no push, stop
> and report at about 80 tool calls. Go 1.22 language level, no new dependencies (`go.mod` stays empty of
> requires; Swift package stays dependency-free). Do not touch `HighSpeedProxyEngine.kt`, other Android
> code, the legacy `desktop/main.go`, or tasks outside this brief. Write the listed tests first and run
> at least one against the unfixed code to record that it fails; report that output. Never weaken an
> assertion to pass: an existing test may change only where the brief says so, and for the reason given.
> No hard-coded expected values or test-only production branches. Injection seams are unexported and nil
> in production. Tokens never appear in logs, errors, reports or test output. Add no text claiming speed
> or Steam adoption. Update the package's `API.md` or README when a stated contract changes. Report the
> commit SHA, exact commands, pass/fail output, and anything you could not run. Windows-only tests run in
> CI, so say they were only compiled locally (`GOOS=windows GOARCH=amd64 go vet`).

Each `-run` or `--filter` below is the narrow check. The wave's CI push is the broader one.

---

### R1. Same-serial USB reconnect (F1, C1)

- **Files:** `desktop/internal/usbmux/{client.go,forwarder.go,API.md}` and their tests.
- **API:**
  - `func (c *Client) ConnectSerial(ctx context.Context, serial string, port uint16) (net.Conn, Device, error)`
  - `func (f *Forwarder) Status() ForwarderStatus`
  - `type ForwarderStatus struct { Serial string; LastDeviceID uint32; Reenumerations, ConnectFailures uint64 }`
  - Share the handshake with `Connect` through an internal `pick` function. `Connect` behaviour stays
    identical. `Forwarder.serveConn` uses `ConnectSerial`.
- **Permitted test change:** `TestForwarderDoesNotSwitchAfterDisconnect` uses the same serial with ID 8,
  which is now the supported case. Change its second session to a **different serial** and keep the
  "no Connect sent, no bytes" assertion. `TestConnectDoesNotFallback` stays untouched.
- **New tests:** `TestConnectSerialUsesFreshIDOnSameControlConn` (asserts the DeviceID on the wire; add an
  `expectConnectID` helper), `TestConnectSerialRefusesOtherSerialAndNetworkEntry`,
  `TestConnectSerialSelectsUSBWhenSameSerialAlsoNetwork`, `TestConnectSerialDuplicateUSBEntriesFailClosed`,
  `TestConnectSerialEmptySerialNeverDials`, `TestForwarderReconnectsSameSerialNewID`,
  `TestForwarderStatusCountsReenumeration`.
- **Check:** `cd desktop && go test -race -count=3 ./internal/usbmux && GOOS=windows GOARCH=amd64 go vet ./internal/usbmux`
- **Accept:** all of C1 is asserted on the wire. `TestForwarderReconnectsSameSerialNewID` fails on the old code.
- **Hardware residual:** H2.

### R2. Transactional hosts library (F7, C4)

- **Files:** `desktop/internal/steamsetup/{hosts.go,journal.go,retry.go,retry_windows.go,retry_other.go}`,
  tests, new `hosts_windows_test.go`. Do not touch `cmd/`.
- **API:**
  - `Update(path, ip, lease string) (bool, error)`: same signature, follows C4.
  - `UpdateWithRetry(ctx context.Context, path, ip, lease string, p RetryPolicy) (bool, error)`
  - `RetryPolicy{Initial, Max, Total time.Duration}`, `IsTransient(error) bool`, `ErrNeedsManualRecovery`
  - `Inspect(path string) (Inspection, error)` with
    `Inspection{Leases []LeaseEntry{Lease, IP string}; ForeignOverride, Malformed bool; PendingJournals []string}`. Inspect never writes.
  - Unexported `var faultHook func(stage string)` with stages `after-journal`, `mid-write`, `after-write`, `after-sync`.
- **New tests (all platforms):**
  - `TestCrashAtEachStageRecoversOnNextUpdate` uses a real subprocess that exits inside the hook, for install and removal.
  - `TestRecoveryPreservesForeignAppendAfterApply`
  - `TestRecoveryRefusesForeignEditOfTornFile` (bytes unchanged, journal kept, typed error)
  - `TestUnresolvedJournalBlocksLaterUpdate`
  - `TestTornJournalTempIgnored`, `TestJournalRemovedOnSuccess`
  - `TestRefusalIsNeverRetried`, `TestRetryHonoursTotalBudget`
  - `TestInspectReportsLeasesOverridesAndJournalsWithoutWriting`
  - `TestUpdateKeepsFileIdentity` (`os.SameFile`)
- **New tests (Windows, real behaviour):**
  - `TestUpdatePreservesExplicitDACL`: set a DACL with `icacls`, compare `(Get-Acl).Sddl` from `powershell.exe` before and after.
  - `TestForeignWriterBlockedDuringUpdate`: from the `mid-write` hook, a second handle's write must fail with errno 33.
  - `TestTransientLockThenSuccess`: a second process holds `LockFileEx`, then releases it.
- **Check:** `cd desktop && go test -race -count=3 ./internal/steamsetup && GOOS=windows GOARCH=amd64 go vet ./internal/steamsetup`
- **Accept:** the crash test fails on the old code. Existing `hosts_test.go` passes unchanged.
- **Hardware residual:** H7.

### R3. Relay path health and HTTP body idle (F3)

- **Files:** `desktop/internal/steamaccel/{relay.go,tls.go,API.md}` and tests.
- **TLS:**
  - `pipeTLS` returns `tlsOutcome{bytes uint64; cause tlsCause}`, with a set-once first cause, recorded
    **before** either socket is closed: `causeClean`, `causeClient`, `causeUpstream`, `causeUploadStall`, `causeCancelled`.
  - The path is marked failed only for: upstream read/write error, upstream EOF with zero downstream
    bytes, or an idle expiry whose last progress was **client→upstream**.
  - Idle after downstream progress is keep-alive, and client errors and cancellation are not path failures.
  - Still never retry after a ClientHello byte was sent.
- **HTTP:**
  - Each attempt gets its own cancellable context. Body reads run under an idle timer that is armed only
    while blocked in `Read`, and firing it cancels the attempt.
  - Each downstream write sets `http.ResponseController.SetWriteDeadline(now+IdleTimeout)`, tolerating
    `ErrNotSupported`. A stalled client is not a path failure. A stalled origin is, with cooldown and
    `ErrAbortHandler`. Never retry or splice after commit.
- **Permitted doc change:** remove "TLS stream errors after forwarding do not diagnose/poison uplink health" from `API.md`.
- **New tests:** `TestTLSUpstreamResetCoolsDownPath`, `TestTLSUpstreamEOFBeforeFirstByteCoolsDownPath`,
  `TestTLSUploadStallCoolsDownPath`, `TestTLSKeepaliveIdleDoesNotPoisonPath`,
  `TestTLSClientResetDoesNotPoisonPath`, `TestTLSCancelDoesNotPoisonPath`,
  `TestHTTPBodyStallAbortsAndCoolsDown`, `TestHTTPBodyStallNeverRetriesOrSplices`,
  `TestHTTPSlowClientDoesNotPoisonPath`, `TestHTTPBodyProgressOutlivesIdleTimeout`.
- **Check:** `cd desktop && go test -race -count=3 ./internal/steamaccel && GOOS=windows GOARCH=amd64 go vet ./internal/steamaccel`
- **Accept:** a stalled body no longer holds a slot beyond `IdleTimeout`, and `TestTLSUpstreamResetCoolsDownPath` fails on the old code.
- **Hardware residual:** H5.

### R4. iOS peer policy and protocol version (F4, C3)

- **Files:** `ios/Sources/ProxyCore/{ProxyServer.swift,HTTPProtocol.swift,PublicAddress.swift}`, `ios/Tests/`,
  `integration/USBHostFixture.swift`, new `integration/public-ip-vectors.tsv`,
  new `desktop/internal/steamaccel/security_vectors_test.go` (test only), `ios/README.md`.
- **API:**
  - `PublicAddress.allows(_ endpoint: NWEndpoint?) -> Bool`
  - `EgressPolicy.peerAllowed` as a **required** init argument with no default, so every fixture states its policy; production `.cellular` uses `PublicAddress.allows`.
  - `ProxyRoute.info`; `ProxyProtocol.version = 2`; `GET /diagnostic/info` returns `{"protocol":2}` with a Content-Length, token required.
- **Vector file:** columns `address  go  swift`, with values `allow|deny`. Port the full Go deny list and
  the `private` and public lists from `TestSecurityHostsPeersDNSAndPorts`. Go reads
  `../../../integration/public-ip-vectors.tsv`; Swift resolves it from `#filePath`. Do not change Go production code.
- **Permitted test change:** fixtures that use a loopback peer pass `peerAllowed: { _ in true }`. This
  mirrors the existing `permits` override.
- **New tests:** `testPublicAddressVectors`, `TestPublicIPVectors` (Go),
  `testNonPublicLiteralRejectedBeforeAnyConnection` (the `connection` closure is never called),
  `testNonPublicPeerGets403AndNoBytesReachPeer`, `testNilOrNamedPeerFailsClosed`,
  `testHostnameConnectReturns200WhenPeerAllowed`, `testLegacyNumericHostsRejected`,
  `testProductionPeerPolicyRejectsLoopbackRemoteEndpoint`, `testDiagnosticInfoBodyAndAuth`.
- **Check:** `swift test --package-path ios && (cd desktop && go test -count=1 -run TestPublicIPVectors ./internal/steamaccel) && TF_INTEGRATION_COUNT=3 ./integration/verify-host.sh`
- **Accept:** `testDeniedEgressNeverReturnsCONNECT200` and the cellular-only policy are unchanged.
- **Hardware residual:** H1, H3.

### R5. Guardian retry and recovery wiring (F7, C4). Needs R2.

- **Files:** `desktop/cmd/steam-accelerator/{platform_windows.go,platform_other.go,recovery.go,recovery_test.go,guardian_windows_test.go}`, minimal `main.go` hunks.
- **Change:**
  - `guard` delegates to `guardPath(pid int, lease string, handle uint64, hostsFile string, flush func() error, recoveryLog string) error`.
  - Add `--recovery-log`, passed to the guardian and defaulting to a file next to the report.
  - Budgets per C4. `reclaimStale(path, homeIP string)` runs after ports are bound and before install.
  - `signal.NotifyContext` also handles `syscall.SIGTERM`, which is how Go reports a console close on Windows.
- **New tests:**
  - `TestReclaimStaleOnlyWhenIPMatchesHome`, `TestReclaimRefusesMalformedOrForeignBlock`, `TestRecoveryLogHasLeaseAndNoToken`.
  - Windows, using the real inherited-handle setup and a temp hosts file: `TestGuardianRemovesBlockAfterForcedParentKill`
    (the parent is killed, not exited; foreign bytes survive), `TestGuardianRetriesTransientLock`,
    `TestGuardianPermanentRefusalWritesRecoveryLog`.
- **Check:** `cd desktop && go test -race -count=3 ./cmd/steam-accelerator && GOOS=windows GOARCH=amd64 go vet ./cmd/steam-accelerator`
- **Accept:** existing `TestInheritedGuardianSurvivesImmediateParentExit` is unchanged. Windows results come from wave CI.
- **Hardware residual:** H7.

### R6. Live byte accounting and uplink modes (F5). Needs R3.

- **Files:** `desktop/internal/steamaccel/{relay.go,tls.go,config.go,API.md}` and tests.
- **API:**
  - `type UplinkMode int` with `UplinkDual`, `UplinkHomeOnly`, `UplinkPhoneOnly`
  - `Config.Uplinks`; `Status.Mode`
  - `PhoneToken` is not required in `UplinkHomeOnly`. Unknown modes are rejected.
  - `acquire` never selects a disabled path, so single-path modes have no failover.
  - Bytes are added per downstream write. `finish` no longer takes a byte count.
  - Coordinate the `acquire` hunk with R7.
- **New tests:** `TestHTTPBytesVisibleBeforeBodyCompletes`, `TestUplinkHomeOnlyNeverDialsPhone`,
  `TestUplinkHomeOnlyValidWithoutToken`, `TestUplinkPhoneOnlyNeverDialsHomeAndFailsClosed`,
  `TestTLSRespectsUplinkMode`, `TestConfigRejectsUnknownUplinkMode`.
- **Check:** `cd desktop && go test -race -count=3 ./internal/steamaccel`
- **Accept:** integration byte accounting (`Home+Phone == len*30`) still holds, via `TF_INTEGRATION_COUNT=3 ./integration/verify-host.sh`.

### R7. Relay token refresh (F2, C2). Needs R3.

- **Files:** `desktop/internal/steamaccel/{connect.go,relay.go,tls.go,config.go,phone_auth.go,API.md}` and tests.
- **API:**
  - `func (r *Relay) SetPhoneToken(token string) error`
  - `func NormalizePhoneToken(raw string) (string, error)`
  - `Status.PhoneAuth PhoneAuthStatus{Stale bool; Generation, Rejections uint64; StaleSince time.Time}`
  - Internal: `errPhoneAuth`. `connectPhone` reads token and generation together, and a 407 becomes `markPhoneAuthStale(generation)`.
- **Classification:** `errPhoneAuth` joins the local-capacity class for health, so no `Errors++` and no
  cooldown. `dual` mode continues on home in the same request.
- **Permitted test change:** `TestTLSPhoneCONNECTFailureFallsBackHome` uses a 407 to assert a cooldown.
  407 now has its own defined behaviour, so change that fixture to **502** with assertions intact, and
  cover 407 in the new test.
- **New tests:** `TestTLSPhone407FallsBackHomeMarksStale`, `TestPhone407MarksStaleWithoutCooldown`,
  `TestPhoneStalePathNotDialedUntilRefresh`, `TestSetPhoneTokenAppliesToNextCONNECTAndClearsStale`,
  `TestStale407FromOldGenerationIgnored`, `TestSetPhoneTokenRejectsInvalidKeepsOld`,
  `TestSetPhoneTokenClosesIdleKeepsActive`, `TestNormalizePhoneToken`, `TestStatusNeverContainsToken`.
- **Check:** `cd desktop && go test -race -count=3 ./internal/steamaccel`
- **Accept:** a generation race test runs under `-race`. Existing `connect_test.go` token-leak checks stay.

### R8. iOS token session and clean restart (F2, C2). Needs R4.

- **Files:** `ios/Sources/ProxyCore/{TokenSession.swift,ProxyServer.swift}`, `ios/Tests/ProxyCoreTests/TokenSessionTests.swift`, `TransportTests.swift`, `ios/App/TetherFlowApp.swift`, `ios/README.md`.
- **API:** a pure `TokenSession` struct with injected generator and clock, and events `userStart`,
  `userStop`, `userRotate`, `scene(active:)` returning `.startServer(token)`, `.stopServer` or `.none`.
  Add `TokenFormat.generate(bytes:)` and `TokenFormat.grouped(_:)`. `ProxyModel` stays a thin adapter.
  UI: grouped token, Rotate button, "Paused, will resume" status, and README text for the new lifetime.
- **Restart race:** write `testImmediateRestartSamePortAfterStopped` first (50 cycles of stop, wait for
  `.stopped`, start on the same fixed port) and record that it fails. Then make `.stopped` fire only after
  the listener reports `.cancelled` (2-second watchdog), and have `ProxyModel` start only after `.stopped`.
  If that is not enough, add `allowLocalEndpointReuse` and a bounded retry on `EADDRINUSE` (5 × 200 ms),
  using only what is needed for 50/50.
- **New tests:** `testUserStartGeneratesFreshToken`, `testSceneInactivePausesAndResumesSameTokenWithinWindow`,
  `testResumeAfterWindowWipesToken`, `testUserStopWipesToken`, `testRotateIssuesNewToken`,
  `testOldTokenRejectedOnWireAfterRotate`, `testTokenFormatThirtyTwoCrockfordLowercase`. The clock is
  injected, so tests cannot prove the production clock counts through sleep; H4 checks that on the device.
- **Check:** `swift test --package-path ios --filter 'TokenSessionTests|TransportTests' && swift test --package-path ios -c release -Xswiftc -enable-testing`
- **Accept:** a pause closes all sockets, with no background mode added. `Info.plist` settings are unchanged.
- **Hardware residual:** H1, H4.

### R9. Per-path DNS and dial budget (F4, C3). Needs R4, R6, R7.

- **Files:** `desktop/internal/steamaccel/{relay.go,tls.go,connect.go,config.go,API.md}`, tests.
- **Change:**
  - Move lookup inside the attempt loop, home only, in both `ServeHTTP` and `serveTLS`.
  - `dialTarget` has empty `ips` for the phone. Phone `originDial` makes one `connectPhone(host:port)`.
  - Home dials obey C3's candidate budget.
  - Add `releaseUnused(i)` so a rejected resolve is not counted as a Request.
  - Add `errPhoneOriginRejected` for CONNECT 403. Result mapping per C3 item 5.
- **Permitted test change:** `TestDNSRebindingCheckedOnEveryRequest`, the `private` loop in
  `TestSecurityHostsPeersDNSAndPorts`, and the DNS cases in `TestTLSRejectsBeforeDialWithoutWritingAlerts`
  test desktop DNS validation, which now applies to the home path. Set `c.Uplinks = UplinkHomeOnly` and
  keep **every assertion unchanged**. Phone-side rejection is proven by R4 and by the new tests here.
- **New tests:** `TestHomeDNSFailureStillServesViaPhone`, `TestPhoneFailureStillServesViaHome`,
  `TestPhoneCONNECTUsesHostnameAndNeverLocalDNS`, `TestAllowlistRunsBeforePhoneCONNECT`,
  `TestPhone403NoCooldownFallsBackHome`, `TestHomeMixedPrivateDNSRejectedWithoutDial`,
  `TestHomeBlackholedFirstIPStillReachesSecond`, `TestHomeDialCandidateCapAndTotalBudget`,
  `TestRejectedResolveNotCountedAsAttempt`, `TestTLSHomeDNSFailureUsesPhone`, `TestTLSPhoneUsesHostnameCONNECT`,
  `TestPhoneOnlyStaleReturns503NoHomeFallback`.
- **Check:** `cd desktop && go test -race -count=3 ./internal/steamaccel && cd .. && TF_INTEGRATION_COUNT=10 ./integration/verify-host.sh`
- **Accept:** the blackhole test fails on the old code. The cross-language run passes 10/10 in both modes, with no failure dismissed as a flake.
- **Hardware residual:** H3.

### R10. CLI token ingress and refresh, launcher (F2, C2). Needs R5, R7, R8.

- **Files:** `desktop/cmd/steam-accelerator/{token.go,token_test.go,console_windows.go,console_other.go,console_windows_test.go,main.go}`, `desktop/steam-tools/Start-SteamAccelerator.ps1`.
- **Change:**
  - Remove `--phone-token-file`; the flag package then rejects it.
  - `readTokenLine(*bufio.Reader)` is bounded at 4 KiB and discards overlong input without logging it.
  - `withHiddenInput(*os.File, func())` uses `GetConsoleMode`/`SetConsoleMode` via `kernel32` and restores the mode on exit.
    For a non-console stdin it is a no-op.
  - A startup 407 re-prompts at most 3 times.
  - One refresh goroutine: normalize, probe with the candidate token, `SetPhoneToken` only on success.
    Stale state logs one prompt per generation. EOF disables refresh with one log line and the relay keeps running.
  - Launcher: delete the temp-file and `Read-Host` block; the executable prompts through inherited stdin.
- **New tests:** `TestReadTokenLineBoundsAndTrim`, `TestRefreshSwapsOnlyAfterProbeSuccess`,
  `TestRefreshKeepsOldTokenOnProbeFailure`, `TestRefreshEOFKeepsRunning`, `TestStartupRepromptLimit`,
  `TestNoTokenInLogsOrReport`, `TestTokenFileFlagRemoved` (runs the built binary, expects exit 2),
  `TestHiddenInputClearsEchoAndRestores` (Windows: helper with `CREATE_NEW_CONSOLE`, reads the mode via `CONIN$`).
- **Check:** `cd desktop && go test -race -count=3 ./cmd/steam-accelerator && GOOS=windows GOARCH=amd64 go vet ./cmd/steam-accelerator`
- **Accept:** `grep -n 'token' desktop/steam-tools/Start-SteamAccelerator.ps1` shows no file path or secret handling.
- **Hardware residual:** H4.

### R11. CLI modes and true concurrency metric (F5). Needs R6, R10.

- **Files:** `desktop/cmd/steam-accelerator/{main.go,rates.go,rates_test.go}`, launcher.
- **Change:**
  - `--uplink-mode dual|home|phone`, launcher `-Mode Dual|HomeOnly|PhoneOnly`. `home` skips USB and token entirely.
  - `concurrencyTracker` counts an interval only when **both** paths exceed `concurrentFloorMbps = 1` in the same interval.
  - Report fields: `uplink_mode`, `both_paths_active_now`, `concurrent_seconds`, `longest_concurrent_seconds`,
    `both_paths_ever`, `active_seconds`, `sustained_mbps` (per path and combined, over active seconds), `phone_auth`.
  - `both_paths_observed` is **removed**, so old readers cannot mistake it for concurrency.
  - Report writes are temp-then-rename. The launcher includes the mode in the report filename so baselines do not overwrite each other.
  - Fix the `intervalMbps` comment, which still says completion-time accounting.
- **New tests:** `TestConcurrencyRequiresSameIntervalAboveFloor`, `TestHistoricalBytesAreNotConcurrent`,
  `TestStreakResetsOnGap`, `TestSustainedMbpsExcludesIdleTime`, `TestReportFieldsAndNoToken`, `TestParseUplinkMode`,
  `TestHomeModeOpensNoUSBAndPromptsNoToken`.
- **Check:** `cd desktop && go test -race -count=3 -run 'Concurrency|Historical|Streak|Sustained|Report|UplinkMode|HomeMode|IntervalMbps' ./cmd/steam-accelerator`
- **Accept:** a run with sequential home-then-phone traffic reports `concurrent_seconds == 0` and `both_paths_ever == true`.
- **Hardware residual:** H6.

### R12. Doctor, selectable phones, cellular preflight (F6). Needs R1, R4, R9, R11.

- **Files:** `desktop/cmd/steam-accelerator/{doctor.go,doctor_test.go,usb.go,phonecheck.go,phonecheck_test.go,main.go}`, launcher.
- **Change:**
  - `--doctor [--home-ip X] [--json]` prints OK/WARN/FAIL for: interfaces, Apple USB service, USB devices,
    link metadata (WARN at 480 Mbps; still "not throughput"), port 80/443 bind when `--home-ip` is given,
    hosts `Inspect` results with the exact recovery command, and the required app/foreground reminder.
    Exit 2 on any FAIL. No side effects.
  - `--list-phones` prints `serial=<normalized> id=<n> link=<metadata>`, and the serial works with `--device-udid`.
    The serial stays out of the report and logs.
  - Preflight order, all **before** port binding and hosts:
    1. `/diagnostic/info` protocol is 2.
    2. Exact-byte USB loop.
    3. Cellular probe: authenticated `CONNECT <probe-host>:443`, then a verified TLS handshake using system roots.
    - A CONNECT 200 is required and shows a cellular-scoped TCP connect to a validated public peer.
      TLS failure is WARN only, since it may just mean the probe host is wrong. No Mbps is ever printed for this.
    - `--probe-host` defaults to `steampipe.akamaized.net`, must pass the allowlist, and needs confirming at H3.
  - The launcher treats doctor's exit 2 as a printed failure and stops.
- **New tests:** `TestDoctorReportsEachCheck`, `TestDoctorExitCodes`, `TestDoctorDetectsStaleLeaseAndPendingJournal`,
  `TestDoctorHasNoSideEffects`, `TestListPhonesSerialRoundTripsToSelectUSB`, `TestPreflightRejectsOldProtocol`,
  `TestPreflightCellularFailureStopsBeforeBind`, `TestPreflightTLSFailureIsWarnOnly`, `TestPreflightOrder`.
- **Check:** `cd desktop && go test -race -count=3 ./cmd/steam-accelerator && GOOS=windows GOARCH=amd64 go vet ./cmd/steam-accelerator`
- **Accept:** output and docs say the probe shows cellular reachability, not cellular speed.
- **Hardware residual:** H2, H3.

### R13. iOS Release and signing verification. Needs R8.

- **Files:** `ios/tools/{build-device.sh,verify-app.sh,build-signed.sh,test_signing_scripts.py,validate.sh}`, `ios/project.yml`, `ios/Config/Signing.xcconfig.example`, `ios/.gitignore`, `integration/verify-host.sh`, the `mac-host` job in the workflow.
- **Change:**
  - `build-device.sh` builds **Debug and Release** unsigned.
  - `verify-app.sh <app>` fails unless: `vtool` shows platform IOS; no `UIBackgroundModes` or ATS exception in
    `Info.plist`; no entitlements beyond an optional `get-task-allow`; the binary has no `USBHostFixture` or
    `TEST-ONLY` strings; and the App/Sources tree uses no `UserDefaults`, `SecItemAdd`, or file-write API for the token.
  - `project.yml` takes the team and bundle ID from an optional, gitignored local xcconfig.
  - `build-signed.sh` needs `TF_TEAM_ID` and `TF_BUNDLE_ID`, builds Release, and uses `-allowProvisioningUpdates`
    only when `--allow-provisioning` is passed. It then checks `codesign -d --entitlements` and the embedded
    profile. It is run by the user on their own Mac; no agent or CI touches accounts.
  - `verify-host.sh` also runs the fixture built with `swiftc -O`.
  - CI adds `swift test -c release -Xswiftc -enable-testing`, the Release build and `verify-app.sh`.
- **New tests (Python, stubbed `xcodebuild`/`codesign` on `PATH`):** `test_signed_build_refuses_without_team`,
  `test_signed_build_uses_release_and_no_provisioning_by_default`, `test_verify_app_rejects_background_mode`,
  `test_verify_app_rejects_fixture_strings`, `test_verify_app_rejects_extra_entitlement`.
- **Check:** `python3 -m unittest discover -s ios/tools -p 'test_*.py' -v && swift test --package-path ios -c release -Xswiftc -enable-testing`. Then run `ios/tools/build-device.sh`; local Xcode destination discovery is known to be intermittent, so if it fails report that and let CI decide.
- **Accept:** Release is verified by behaviour, not just compiled. No SDK, runtime or account change happens.
- **Hardware residual:** H1.

### R14. Windows end-to-end and package. Needs R12.

- **Files:** `desktop/cmd/steam-accelerator/e2e_windows_test.go`, `desktop/steam-tools/{Test-Launcher.ps1,package.ps1,START-HERE.zh-CN.txt}`, `desktop/steam-tools/testdata/fakeaccel/main.go`, the `windows` job in the workflow.
- **E2E (`TF_E2E_WINDOWS=1` only, because it changes the runner's real hosts file and ports):** build the
  exe with `-trimpath`, and run it from a directory whose path has spaces and CJK characters. Use a fake
  usbmuxd on the production `127.0.0.1:27015`, a Go fake phone speaking the real wire protocol, the runner's
  real hosts file, and `--uplink-mode phone`, so no internet origin is needed.
- **Tests:**
  - `TestE2EPhoneModeDownloadExact`: `Host: cache1.steamcontent.com` to `HomeIP:80`; exact payload; report bytes match.
  - `TestE2EStaleTokenRefreshOverStdin`: 407, stale log, new token on stdin, resumes; no token in any output.
  - `TestE2EReenumerationReconnects`
  - `TestE2EForceKillGuardianCleansHosts`: kill the process; hosts byte-identical except a foreign line added during the run; SDDL equal.
  - `TestE2ECtrlBreakCleanExit`
  - `TestE2EStaleBlockReclaimedAfterGuardianKilled`
- **Launcher, under real Windows PowerShell 5.1 (`powershell.exe -NoProfile -File`) from the extracted
  package in a CJK path:** `Test-Launcher.ps1` uses `fakeaccel`, which records argv and stdin, and asserts:
  - no token in argv;
  - no temp token file;
  - exit code propagation;
  - `-Mode`, `-DeviceUDID`, `-HttpOnly` mapping, and invalid-UDID rejection;
  - `-CheckPhoneOnly` skips the admin path;
  - the `.ps1` is pure ASCII (5.1 reads a BOM-less file as ANSI).
- **Package:** `package.ps1` zips the exe, launcher, README, START-HERE and a MANIFEST (commit, exe SHA-256,
  `Get-AuthenticodeSignature` status, "unsigned experimental"). It replaces the hand-built zip, and CI uploads
  the zip it tested. The real exe's `--doctor --json` is asserted to report the Apple USB service as FAIL on the runner.
- **Check:** `cd desktop && GOOS=windows GOARCH=amd64 go vet ./cmd/steam-accelerator`, then the wave's CI run.
- **Accept:** E2E runs and passes on `windows-latest` with no skips. Any failure, including Defender or
  port-80 behaviour, is reported as found.
  If the runner lacks an RFC1918 IP, fail with a clear message rather than skipping.
- **Not automatable, moved to H7:** non-admin refusal, SmartScreen, execution policy, Mark of the Web.

### R15. Documentation and handoff. Needs everything.

- **Files:** `desktop/steam-tools/README.zh-CN.md`, `docs/STEAM-MORNING-HANDOFF.zh-CN.md`, `ios/README.md`, `ios/VALIDATION.md`, `integration/README.md`, `notes/steam-verification-final.md`, `docs/PROGRESS.md`.
- **Change:** describe the new token lifetime, refresh, `-Mode` baselines, doctor, recovery and journal
  handling, and the Hardware Gates table below. Remove the temp-token and "rotates on every Start" text.
  Cite only CI run IDs the main session verified. Keep the non-claims at the top of this plan unchanged in meaning.
- **Check:** `grep -n 'phone-token-file\|both_paths_observed' -r desktop ios docs integration` returns nothing except historical records.

## 4. Independent review

The diff covers auth, security, routing and hosts data, so it needs independent review before merging or
release. Review does not block implementation or a draft PR.

- Shape: Codex `gpt-6.1-sol`, xhigh, own worktree, one `delegate_task` per round, at most two rounds.
- Base it on the integration tip after R14. Ask it to check C1–C4 as invariants and to return a coverage checklist.
- If it cannot finish, report the coverage gap; never report it as passed.
- Do not have a second model re-verify this whole plan. Verify only what is about to change.

## 5. Hardware gates

Automation cannot close these. The user has no hardware now, and that blocks nothing above. Run them as
one session. Each result is recorded with device, OS, cable and port.

| Gate | Checks | Failure means |
|---|---|---|
| H1 Sign and install | Release signed through `build-signed.sh` on the actual iOS 27 phone with a compatible Xcode; no unexpected permission prompt; app allowed cellular data | C3's `remoteEndpoint` or iOS support needs rework |
| H2 Apple Devices | `--list-phones`, `Connect`, real serial format; replug gives a new DeviceID and traffic returns; phone listed on both USB and Wi-Fi sync selects USB; restart of the Apple service; a second phone is never chosen | C1 assumptions wrong |
| H3 Cellular and DNS | Wi-Fi on: peer and DNS go via carrier, `remoteEndpoint` non-nil; app cellular off: CONNECT fails, no Wi-Fi fallback, also mid-transfer; IPv6-only/NAT64 carrier if available; `--probe-host` is valid | C3 or probe default wrong |
| H4 Token | Control Center swipe resumes without retyping; lock over 10 minutes requires a new token and old one fails; hidden prompt works; refresh under load; typing 32 characters is tolerable | C2 UX or window wrong |
| H5 Steam adoption | `content_log` shows the local cache and IP; which `Host` values arrive (including whether `lancache.steamcontent.com` itself is ever used, which the home path rejects); stalled or reset path recovers | Steam may not use this route at all |
| H6 Baselines | Same game and region in `home`, `phone`, `dual`; compare `sustained_mbps` and `concurrent_seconds`, excluding disk and decompression phases; negotiated USB speed; heat | Sets the real speed story. **No target is assumed; record what happens** |
| H7 Windows | Defender/other security with hosts changes; SmartScreen, execution policy, Mark of the Web; non-admin refusal; forced kill; real power cut with journal recovery; reboot with stale entry; Wi-Fi reconnect or DHCP change while running | C4 or packaging needs change |

## 6. Noted, not scheduled

- **Phone socket lag:** both ends allow 32 sessions, and iOS frees sessions asynchronously, so the 33rd
  socket can be rejected and read as a path failure with a false cooldown. Measure at H6; if real, lower
  the desktop default below 32.
- **Fixed lifetimes:** the Forwarder's 30-minute cap and `RequestTimeout` can cut very long TLS sessions.
  Steam chunks should be short, but this is unmeasured.
- **Open token risk:** anyone holding the token can reach any public host on 80/443 over cellular, because
  the phone has no host allowlist. The new peer check limits this to public destinations only.
- **Unexplained history:** the first cross-language run's 7 unexpected EOFs have no established root cause
  (`integration/README.md`). The 10× CI loop stays, and any recurrence is a defect, not a flake.
- **Experimental status:** the exe remains unsigned, and neither Windows nor iOS has a store or installer path.
