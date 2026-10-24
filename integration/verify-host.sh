#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin ]]; then echo 'Requires macOS Network.framework; not a physical iOS test' >&2; exit 2; fi
work=$(mktemp -d "${TMPDIR:-/tmp}/tetherflow-integration.XXXXXX")
trap 'rm -rf "$work"' EXIT
swiftc -swift-version 5 -parse-as-library ios/Sources/ProxyCore/HTTPProtocol.swift \
  ios/Sources/ProxyCore/ProxyServer.swift integration/USBHostFixture.swift \
  -o "$work/usb-host-fixture"
cd desktop
TF_SWIFT_FIXTURE="$work/usb-host-fixture" go test -race -count="${TF_INTEGRATION_COUNT:-3}" -v ./integration
