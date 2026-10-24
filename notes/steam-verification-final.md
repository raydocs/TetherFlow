# Verification freeze — 2026-10-24

Code commit: `7a0b54c713e22e665807298662a8f67e11e322fd`.
WIP branch: `wip/steam-ios-dual-download`. Main and existing Android changes untouched.
Later documentation-only commits do not change the tested/reviewed code.

## Exact-code evidence checked by the main session

- GitHub run https://github.com/raydocs/TetherFlow/actions/runs/40083910486 : **success**, head SHA matches code commit.
- Windows real runner: race×3 all four new Go packages, vet, EXE build; PowerShell syntax; read-only doctor. Includes inherited guardian process-handle regression. No Apple Devices/physical iPhone in this runner.
- macOS real runner: 19 Swift tests (zero failures), 5 Python tests, cross-language10 iterations×2 modes (direct and native USBmux protocol fixture), unsigned exact generic iOS scheme **BUILD SUCCEEDED**.
- Independent review R2 (fresh `gpt-6.1-sol`, xhigh, isolated worktree): **approve**, no P1/P2/P3 findings. R1 guardian and phone-budget issues resolved; additionally covered TLS, USBmux and Swift lifetime changes. Review is code review, not actual hardware approval.
- Local extra real Apple macOS USBmux ListDevices successfully decoded; no USB iPhone present, so CLI correctly failed selection. This is not Windows Apple Devices compatibility or Connect evidence.
- Existing Android unowned diff SHA256 remains `c2ee914f58e2957aa5aceb844ba55f3a02d5aa79365ed7d4dc5b3b4eb4380d3f`.

## Windows package

Downloaded the actual Windows artifact from this exact CI run; added START-HERE and provenance/hash manifest, zip integrity checked.
Local: `build/deliverables/TetherFlow-Steam-Windows-7a0b54c-experimental.zip`.
EXE SHA256: `f36afeb13fb02100ada2f179ee568139e760f3930e71b62cc890eec036cf3671`.
Unsigned console EXE, not a signed installer. No signed iOS app included. Do not describe it as a ready-to-use verified1200Mbps release.

## Remaining actual-objective gates — NOT ACHIEVED

1. Sign/install/run iOS app on user's actual iOS27 phone with compatible Xcode, choose intended signing Team/device.
2. Windows Apple Devices native USBmux ListDevices/Connect, USB3 negotiated path and authenticated foreground phone interoperability.
3. Real cellular-only upstream (including disabling cellular while Wi-Fi remains available), uninterrupted exact payload and disconnect recovery.
4. Current Steam local content-server adoption and simultaneous independent home + cellular data during a real game download.
5. Comparable home-only, phone-only, dual-path sustained download evidence; target near phone1200Mbps, not merely two nonzero counters. Exclude CDN/decompression/disk bottlenecks.
6. Windows security-tool behavior and real forced-exit cleanup.

There is no USB-connected phone in this session. CoreDevice had zero connected devices.
There is an Apple Development identity, but no Team selection, profile/account changes, signing upload or device deployment was performed.
Devin verification request failed403 `out_of_quota`; no simulator/UI evidence exists and no local runtime was installed.
Full user goal remains unverified, not completed. See `docs/STEAM-MORNING-HANDOFF.zh-CN.md` and Windows setup for one consolidated acceptance session.
