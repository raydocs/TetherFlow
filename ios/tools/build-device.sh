#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p logs
xcodegen generate --spec project.yml 2>&1 | tee logs/xcodegen.log
xcodebuild -project TetherFlowIOS.xcodeproj -scheme TetherFlowIOS \
  -configuration Debug -destination 'generic/platform=iOS' \
  -derivedDataPath DerivedData CODE_SIGNING_ALLOWED=NO build 2>&1 | tee logs/xcodebuild.log
echo 'PASS: unsigned generic iOS scheme build (not a signed install/device test)'
