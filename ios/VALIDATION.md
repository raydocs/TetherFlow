# Validation evidence and remaining gates

Date: 2026-10-24 (America/Denver). Scope: **only new `ios/` files**.
No commit, push, external agent/reviewer, simulator/emulator run, or runtime
installation was performed. Other workers' changes outside `ios/` are not part
of this evidence or a claim of this implementation.

## Environment actually observed

- Xcode **26.6**, build **17F113** (`/Applications/Xcode.app`).
- `xcodebuild -showsdks`: installed **iPhoneOS 26.5** SDK (not iOS 27).
- Apple Swift **6.3.3**, macOS arm64 host; sources use Swift 5 language mode.
- XcodeGen **2.46.0** already installed; Python 3 already installed.
- No `iproxy` executable found. No signed-device/USB forwarding test was run.

## Results

1. **PASS — host Swift XCTest**: `swift test --package-path ios`.
   `logs/swift-test.log`: 17 tests, zero failures (final repetition copied from
   `logs/repeat-after-fixture-fix/swift-10.log`). These are real macOS host
   tests, not simulator or device tests. No external network is used.
   - All header split boundaries, one-byte fragments, binary trailing CONNECT
     bytes, exact 16 KiB header limit, oversized/incomplete headers/input chunks.
   - Malformed/duplicate/folded/control-byte headers and ambiguous body framing;
     auth failures; destination port 80/443 acceptance and rejection of other ports;
     IPv6 authority and invalid targets; diagnostic bounds and unsupported `/pac`.
   - Actual loopback TCP diagnostic transfer: **2,097,289 bytes**, exact continuous
     offset % 251 pattern, Content-Length, EOF, and partial request delivery.
   - Actual CONNECT :443 relay: **2,097,174 bytes** echoed (5-byte pre-read prefix
     plus 2,097,169-byte upload), equality and both TCP half-closes. An injected
     loopback egress policy replaces cellular **only inside the host fixture**.
     It does not prove production cellular or Internet behavior.
   - Actual CONNECT :80 raw HTTP-shaped bytes, denied path returns 502 not 200,
     production path validator rejects an actual non-cellular loopback path,
     stop cancels accepted partial requests, 32 concurrent active CONNECT tunnels
     (alternating :80/:443, with an echoed byte on each) and 33rd
     connection rejection. This is not a 32-way throughput benchmark.
2. **PASS — Python CLI tests**: `python3 -m unittest discover -s ios/tools -p 'test_*.py' -v`.
   `logs/python-test.log` covers partial/malformed response parsing, an actual
   TCP CLI download and HTTP :80 CONNECT against local fixtures, and deliberate payload corruption
   producing a nonzero exit. Any fixture throughput number is NOT a phone result.
3. **PASS — XcodeGen**: reproducible project from `project.yml`; generated project
   is ignored, no hand-edited project is required. `logs/xcodegen.log`.
4. **PASS recorded — exact unsigned generic iOS device scheme build; destination discovery is intermittent**:

   ```sh
   xcodebuild -project ios/TetherFlowIOS.xcodeproj -scheme TetherFlowIOS \
     -configuration Debug -destination 'generic/platform=iOS' \
     -derivedDataPath ios/DerivedData CODE_SIGNING_ALLOWED=NO build
   ```

   `logs/xcodebuild-generic-success.log` (verbatim build-section extraction from
   `logs/validation-run.log`) ends `** BUILD SUCCEEDED **`. It was run from `ios/`
   by `tools/validate.sh`. A later identical build failed destination discovery
   (`logs/xcodebuild-generic-later-failure.log`), so local repeatability remains
   an environment risk, not an unqualified all-runs-pass result. The successful
   generic build compiles
   SwiftUI, Security, Network.framework, the app and core, links and processes
   Info.plist. Output: `DerivedData/Build/Products/Debug-iphoneos/TetherFlowIOS.app`.
   `logs/binary-platform.log` records **Mach-O arm64**, platform **IOS**, minimum
   OS 17.0, SDK 26.5, and codesign reporting **not signed at all**. This is not a
   simulator binary or evidence of signed installation/physical operation.

`tools/validate.sh` runs all current checks; `tools/build-device.sh` requires a
successful generic scheme build (no fallback or auto-install). Known Xcode
warnings are no AppIntents dependency and interface-orientation metadata
(not a network/build failure).

## Failures encountered and follow-up

- Initial core-only run preceded creation of test files and reported `no tests
  found`; it was not counted as a test pass (`logs/swift-test-initial.log`).
- First complete run recorded errors because the test helper wrapped an expected
  parser throw inside XCTest's autoclosure `XCTUnwrap`, which independently
  records a failure. The helper now evaluates the throwing parser first; malformed
  inputs still throw and are asserted, not accepted or special-cased.
- The first cancellation test raced before listener acceptance. The implementation
  retains its rejection handler during stop and starts rejected setup-state
  sockets on the serial queue before cancellation. Separate tests now check both
  accepted-session stop and **10 iterations without waiting for acceptance**;
  the 33rd-socket test checks the resource-cap rejection path.
  Original failed run: `logs/swift-test-first-with-tests.log`. No claim of
  “fails on old code” was made without a run.
- Initial generic destination discovery reported `Unable to find a destination
  matching the provided destination specifier` and `iOS 26.5 is not installed`.
  A scheme retry using `-sdk iphoneos -arch arm64` also failed with `Found no
  destinations` (`logs/xcodebuild-sdk.log`). An explicit SDK/arm64 **target**
  build passed (`logs/xcodebuild-target.log`); a target command with a destination
  warned it was ignoring that destination (`logs/xcodebuild-target-generic.log`).
  Neither was counted as a passing generic build. Later the **exact generic
  scheme** command succeeded (`logs/xcodebuild-generic-success.log`) on the same
  installed toolchain, with no component installation/change by this task.
  A subsequent exact generic scheme attempt failed the same destination check
  (`logs/xcodebuild-generic-later-failure.log`). The cause is not established;
  success was not inferred from the target-only build. The final additional
  attempt also failed the same destination check and is retained independently
  in `logs/xcodebuild-generic-final.log`. Thus a genuine generic build success
  is recorded, but the latest recheck is **blocked** by local destination discovery.
- Repeated host runs caught a real truncated echo response on run 3:
  2,080,773 bytes instead of 2,097,174 (`logs/repeat/swift-3.log`). The reference
  echo fixture was cancelling its connection immediately after sending final
  data+FIN. It now retains the FIN-sent connection until the client drains it;
  fixture `stop()` does cleanup. No payload expectation or production relay
  behavior was weakened. **10/10 complete 17-test runs passed** after this
  fixture-lifetime correction (`logs/repeat-after-fixture-fix/swift-1.log` through
  `swift-10.log`). This is evidence consistent with early fixture cancellation,
  not a claim of independently proven kernel root cause or physical-iOS safety.
- One earlier intermediate 16-test run returned one failure and its detailed line
  was lost when a log path was reused. Its cause cannot be established from that
  log. It is not silently declared to be the same fixture issue. Subsequent
  repeat logs and their failures are preserved separately.

## Physical / integration checklist — NOT RUN

- [ ] Sign/install/run with an Xcode version supporting the user's actual iOS 27
      device; verify OS/device identity rather than assuming the supplied name.
- [ ] Pair/trust, USB-only iproxy loopback connection to phone 8283; identify Windows
      driver/service and forwarding binary versions. Confirm no Wi-Fi sync path.
- [ ] Run authenticated diagnostic at several sizes; verify exact bytes, failure
      on wrong token, and refusal from Wi-Fi/LAN addresses. Record phone heat and
      host/phone memory for 32 live tunnels with a slow consumer.
- [ ] With Wi-Fi enabled and cellular available, prove CONNECT :80/:443 takes
      cellular using controlled endpoint/routing/counter evidence. Then disable
      cellular **for this app** while Wi-Fi remains usable: no tunnel may succeed
      or fall back. Test disabling it mid-transfer, lost coverage, and timeouts.
      Host tests check policy code but cannot establish physical egress or DNS.
- [ ] Stop, lock, switch apps/control center, disconnect cable, kill host client,
      start again: sockets close, activity does not resume in background, old
      token fails, new token works. Check battery/thermal behavior on real device.
- [ ] Confirm end-to-end TLS certificate verification through HTTPS CONNECT;
      confirm raw HTTP CDN :80 through the authenticated Windows relay.
- [ ] Integrate Windows' separate relay/lancache-style routing, auth and CDN request
      scheduling. Existing Android desktop 8282/PAC/ADB path stays incompatible.
- [ ] Independently measure cellular Internet payload, USB-only transport, and
      combined Windows home-Wi-Fi + cellular Steam downloads. Record baselines,
      repeated runs, carrier accounting/data policy and bottlenecks. **No 1200 Mbps,
      10 Gbps forwarding, bonding, Steam acceleration or billing bypass claim.**

## Ownership check

The pre-existing `HighSpeedProxyEngine.kt` diff SHA-256 was
`c2ee914f58e2957aa5aceb844ba55f3a02d5aa79365ed7d4dc5b3b4eb4380d3f` at inspection
and unchanged at verification. No files outside `ios/` were edited by this task;
no unrelated changes were staged, reverted or claimed. No independent security
review was requested/run; this prototype has not been merged or deployed.
