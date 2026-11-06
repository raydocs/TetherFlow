#!/bin/bash
# Unsigned generic-device builds of the explicit TetherFlowIOS scheme, Debug and Release.
# Never substitutes a target-only build and never changes SDKs, runtimes or accounts.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p logs
xcodegen generate --spec project.yml 2>&1 | tee logs/xcodegen.log
for config in Debug Release; do
  lower=$(printf '%s' "$config" | tr '[:upper:]' '[:lower:]')
  xcodebuild -project TetherFlowIOS.xcodeproj -scheme TetherFlowIOS \
    -configuration "$config" -destination 'generic/platform=iOS' \
    -derivedDataPath DerivedData CODE_SIGNING_ALLOWED=NO build 2>&1 | tee "logs/xcodebuild-$lower.log"
done
cp logs/xcodebuild-debug.log logs/xcodebuild.log
echo 'PASS: unsigned generic iOS scheme builds, Debug and Release (not a signed install/device test)'
