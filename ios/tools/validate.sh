#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p logs
{
  date -u
  xcodebuild -version
  xcodegen --version
  swift --version
} 2>&1 | tee logs/toolchain.log
swift test 2>&1 | tee logs/swift-test.log
swift test -c release -Xswiftc -enable-testing 2>&1 | tee logs/swift-test-release.log
python3 -m unittest discover -s tools -p 'test_*.py' -v 2>&1 | tee logs/python-test.log
./tools/build-device.sh
./tools/verify-app.sh DerivedData/Build/Products/Release-iphoneos/TetherFlowIOS.app 2>&1 | tee logs/verify-app.log
