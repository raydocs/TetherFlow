#!/usr/bin/env bash
# Idempotent repository bootstrap for TetherFlow Cloud Agents.
#
# System toolchains (JDK 17, Android SDK 36, Go 1.22) are provided by
# .cursor/Dockerfile. This script only refreshes repository-dependent state:
# the Gradle distribution/dependency cache and Go module cache. It is safe to
# run repeatedly.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== Toolchain versions =="
java -version
go version
echo "ANDROID_HOME=${ANDROID_HOME:-<unset>}"

echo "== Warming Gradle build (downloads distribution + dependencies, compiles app) =="
./gradlew assembleDebug --no-daemon --console=plain

echo "== Verifying Go desktop companion compiles (host target) =="
( cd desktop && go vet ./... )

echo "== Install complete =="
