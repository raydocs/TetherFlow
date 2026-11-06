#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin ]]; then echo 'Requires macOS Network.framework; not a physical iOS test' >&2; exit 2; fi
work=$(mktemp -d "${TMPDIR:-/tmp}/tetherflow-integration.XXXXXX")
trap 'rm -rf "$work"' EXIT
# The same fixture is built unoptimized and with -O, so optimizer-only behaviour differences show up.
swiftc -swift-version 5 -parse-as-library ios/Sources/ProxyCore/HTTPProtocol.swift \
  ios/Sources/ProxyCore/ProxyServer.swift integration/USBHostFixture.swift \
  -o "$work/usb-host-fixture"
swiftc -O -swift-version 5 -parse-as-library ios/Sources/ProxyCore/HTTPProtocol.swift \
  ios/Sources/ProxyCore/ProxyServer.swift integration/USBHostFixture.swift \
  -o "$work/usb-host-fixture-O"
cd desktop
for fixture in usb-host-fixture usb-host-fixture-O; do
  echo "== Go to Swift transport tests with $fixture"
  TF_SWIFT_FIXTURE="$work/$fixture" go test -race -count="${TF_INTEGRATION_COUNT:-3}" -v ./integration
done
