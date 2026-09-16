# Steam dual-network download mode (Windows)

This opt-in companion distributes Steam TCP connections on ports 80/443 between
home Wi-Fi and the phone's HTTP proxy. It uses the upstream **mihomo v1.19.31** TUN
engine to capture connections that ignore Windows PAC settings. The existing
`tetherflow-win.exe` tray menu does **not** activate this mode.

This is connection load balancing, not single-connection bonding. It cannot
promise the sum of both link speeds: CDN behavior, disk unpacking, phone CPU and
the number of active connections all matter. Round-robin is not adaptive
bandwidth scheduling. Other apps and Steam's non-HTTP(S) traffic use home Wi-Fi.

## Weighted balancing and the ADB tunnel

Egress nodes are replicated to approximate a bandwidth ratio: by default the
phone path gets **5 replicas** and home Wi-Fi **1** (`-PhoneWeight 5
-HomeWeight 1`), because on the reference setup the ADB-tunnel phone path
sustains ~1010 Mbps across 16 streams (864 Mbps over RNDIS) while home Wi-Fi
sustains ~200 Mbps. Tune the ratio after benchmarking your own links; the sum
of both weights is capped at 16.

If the phone is ADB-authorized (Developer options -> USB debugging, accept the
"Allow USB debugging" dialog once), the scripts automatically install Google
platform-tools and switch the phone leg to the loopback ADB tunnel
(`127.0.0.1:8282`). The tunnel bypasses the RNDIS per-flow cap and, in this
mode, the USB-tethering adapter is blocked entirely by the firewall guard (the
phone is reached over the ADB USB interface, not over IP tethering). Pass
`-NoAdbTunnel` to stay on the USB-tethering gateway path; USB tethering can
even stay disabled in tunnel mode.

To capture other big downloaders besides Steam, pass process names:

```powershell
.\Start-SteamDual.ps1 -ProcessNames steam.exe,steamwebhelper.exe
```

## Start

1. Connect Windows to home Wi-Fi. Keep Android USB tethering enabled, and start
   TetherFlow on the phone. Its proxy must be reachable at the USB gateway on 8282.
2. Pause Steam downloads before switching modes.
3. In **PowerShell 7 as Administrator**, run from this directory:

   ```powershell
   .\Start-SteamDual.ps1
   ```

   For different adapter names or a nonstandard phone address:

   ```powershell
   .\Start-SteamDual.ps1 -HomeInterface 'Wi-Fi' -PhoneInterface 'Ethernet 3' -PhoneAddress '10.81.25.145'
   ```

4. Wait for the core's TUN-start message, then resume Steam. Existing TCP sessions
   do not migrate; pause/resume (or restart Steam) is necessary after enabling TUN.
5. Run `Get-SteamDualStatus.ps1` in another PowerShell. Look for Steam connections
   through **HOME-WIFI** and **PHONE-8282** with increasing download byte counts.
   Both network adapters being connected, or a tray notification, is not proof.

The first run downloads the official Windows amd64-compatible mihomo archive,
checks a pinned SHA-256, and extracts it under `%LOCALAPPDATA%\TetherFlow\SteamDual`.
No system-wide proxy setting or startup entry is changed by these scripts.
Do not run alongside another TUN/VPN without resolving routing conflicts first.
The old PAC-based companion can be exited before starting this mode.
`Launch-SteamDual.ps1` is the optional UAC/background launcher; startup errors
are recorded in `%LOCALAPPDATA%\TetherFlow\SteamDual\launcher.log`.

## Phone path and persistent leak guard

The phone node is an HTTP proxy bound to the USB interface. It never falls back
to direct USB internet. A Windows Firewall outbound block on that adapter also
blocks every remote IPv4 address except the phone itself, and blocks IPv6. This
prevents Steam's old or bypassing connections from using ordinary USB internet.
All firewall profiles must be enabled; startup fails otherwise.

The guard remains when the core exits or crashes. This is intentional. Wi-Fi is
the only direct internet path; if unavailable, connections may fail instead of
silently using ordinary tethering. Do not assume use of the Android proxy proves
anything about how a carrier bills or classifies traffic.

Ctrl+C stops the foreground core. To allow ordinary USB tethering again, stop
the core and explicitly run as Administrator:

```powershell
.\Remove-UsbGuard.ps1
```

For a hidden background core, use `Start-SteamDual.ps1 -Background`; stop it with
`Stop-SteamDual.ps1` as Administrator. Logs are saved in the same local state
directory as `core-out.log` and `core-err.log`. A running process alone does not
prove TUN initialized: verify the startup log and Steam connection paths.

The guard is scoped to the selected adapter name. Rerun setup if Windows creates
a different USB adapter or if the phone's USB IP changes. The feature is not a
system-wide fail-closed VPN and does not protect unrelated adapters.
The USB IPv4 interface metric is raised above the home default-route cost so
Wi-Fi remains usable when the core stops. Its previous metric is saved and
restored by `Remove-UsbGuard.ps1`, together with removal of the guard.

## Validation without changing routes

```powershell
.\Test-SteamDual.ps1
.\Start-SteamDual.ps1 -PrepareOnly              # generate and validate production config
.\Start-SteamDual.ps1 -ProxyTest                # local proxy only, no TUN/firewall changes
curl.exe --noproxy "" -x http://127.0.0.1:17890 https://www.microsoft.com/ -o NUL
.\Get-SteamDualStatus.ps1 -ProxyTest
```

After a real elevated start, `Test-UsbGuard.ps1` binds test sockets to the USB
interface with `IP_UNICAST_IF`: the phone port must connect, while a public TCP
endpoint must not connect. Pass `-PhoneInterface` and `-PhoneAddress` if needed.

ProxyTest balances all requests explicitly sent to port 17890; it does not
intercept Steam. Passing configuration/unit/proxy checks is not a substitute for
an elevated Steam download test, phone-disconnect test and USB-leak test.

## Verified on 2026-09-16

On Windows with a Samsung USB RNDIS adapter, an ADB-authorized phone and home
Wi-Fi (cachefly 100 MB test file, no disk writes):

- Phone HTTP proxy via USB tethering (RNDIS): 467 Mbps single stream,
  792 Mbps at 4 streams, 844-864 Mbps at 8-16 streams.
- Phone HTTP proxy via the ADB USB tunnel: **1010 Mbps at 16 streams**.
- Full engine chain (`ProxyTest` mode, mihomo, weighted, ADB tunnel phone
  leg + Wi-Fi home leg): **1046 Mbps at 8 streams, 1054 Mbps at 16 streams** -
  the practical sum of both links, so the engine is not the bottleneck.
- Elevated live Steam download peaked at **776 Mbps** in Steam's own
  content_log (above the 200 Mbps home link, proving aggregation) before the
  game SSD throttled it: the library NVMe ran at 0% idle while moving only
  ~40 MB/s. Disk speed, not the network, capped that download.
- Generated production configuration accepted by mihomo v1.19.31.
- Configuration/guard regression checks passed under PowerShell 7.6.6.
- Elevated TUN adapter reached `Up`; the firewall guard installed successfully.
- During a Steam game update, the same 10-second observation window recorded
  19,716,827 bytes on HOME-WIFI and 25,957,936 bytes on PHONE-8282, counting active
  Steam connections. Closed connections are not included in those counters.
- All 35 observed established core TCP connections bound to USB terminated at
  the phone's port 8282, with no public-IP USB connections.
- The pinned-socket guard test passed (phone reachable, direct USB public TCP
  blocked). The home route cost was 35 and the saved/raised USB metric was 135.

The morning's slow Steam update (17 MB/s) was diagnosed as disk-bound unpacking
(C: at 100% busy, queue 15) combined with the old 1:1 split, not an engine
limit; the 4:1 weighting matches the measured link capacities. sing-box was
evaluated as an alternative core but official builds have no load-balance
outbound group (selector/urltest only), which makes per-connection dual-link
aggregation impossible without community forks.

These checks demonstrate simultaneous download paths, not a controlled speedup
benchmark or exhaustive carrier/driver compatibility. Physical unplug/replug,
long-duration throughput and other Windows versions remain to be tested. One
phone-path HTTPS request timed out during preliminary testing; later requests
and Steam payload transfers succeeded.

## Upstream

Mihomo is a separate GPL-3.0 dependency downloaded from
<https://github.com/MetaCubeX/mihomo/releases/tag/v1.19.31>.
Its source and license are at <https://github.com/MetaCubeX/mihomo>.
No mihomo executable is committed or relicensed as part of this repository.

Reference docs: [TUN](https://wiki.metacubex.one/en/config/inbound/tun/),
[load balancing](https://wiki.metacubex.one/en/config/proxy-groups/load-balance/).
