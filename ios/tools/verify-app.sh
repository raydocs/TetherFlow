#!/bin/bash
# Behavioural checks on a built TetherFlowIOS.app (unsigned or signed). Fails closed.
# Usage: verify-app.sh [--signed] [--src-root DIR] <path/to/App.app>
# --signed additionally permits the two identity entitlements Xcode adds when signing
# (application-identifier, com.apple.developer.team-identifier); nothing else is ever allowed.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
src_root="$here"
signed=0
while [[ "${1:-}" == --* ]]; do
  case "$1" in
    --signed) signed=1; shift ;;
    --src-root) src_root="${2:?--src-root needs a directory}"; shift 2 ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done
app="${1:-}"
[[ -n "$app" && -d "$app" ]] || { echo 'usage: verify-app.sh [--signed] [--src-root DIR] <App.app>' >&2; exit 2; }
plist="$app/Info.plist"
[[ -f "$plist" ]] || { echo "FAIL: no Info.plist in $app" >&2; exit 1; }
fail=0
bad() { echo "FAIL: $*" >&2; fail=1; }

# Read one top-level Info.plist key with the stdlib plist parser (handles binary plists).
plist_has() { python3 -I -c 'import plistlib,sys; d=plistlib.load(open(sys.argv[1],"rb")); sys.exit(0 if sys.argv[2] in d else 1)' "$plist" "$1"; }
exe=$(python3 -I -c 'import plistlib,sys; print(plistlib.load(open(sys.argv[1],"rb")).get("CFBundleExecutable",""))' "$plist")
[[ -n "$exe" && -f "$app/$exe" ]] || { echo 'FAIL: CFBundleExecutable missing or not in the bundle' >&2; exit 1; }

# 1. Platform must be iOS (not macOS, Catalyst or simulator).
if ! vtool -show-build "$app/$exe" 2>&1 | grep -Eq '^[[:space:]]*platform[[:space:]]+IOS[[:space:]]*$'; then
  bad "vtool does not report platform IOS for $exe"
fi

# 2. Foreground-only app: no background modes, no ATS exception.
plist_has UIBackgroundModes && bad 'Info.plist declares UIBackgroundModes'
plist_has NSAppTransportSecurity && bad 'Info.plist declares an NSAppTransportSecurity exception'

# 3. Entitlements: only an optional get-task-allow.
ent_err=$(mktemp "${TMPDIR:-/tmp}/tf-verify.XXXXXX")
trap 'rm -f "$ent_err"' EXIT
ent=""
if ! ent=$(codesign -d --entitlements :- "$app" 2>"$ent_err"); then
  if ! grep -q 'not signed at all' "$ent_err"; then bad 'codesign could not read entitlements'; fi
  ent=""
fi
if [[ -n "${ent//[[:space:]]/}" ]]; then
  extra=$(printf '%s' "$ent" | python3 -I -c '
import plistlib,sys
raw=sys.stdin.buffer.read()
try: d=plistlib.loads(raw[raw.index(b"<?xml"):])
except Exception: print("<unparseable entitlements>"); sys.exit(0)
allowed={"get-task-allow"}
if sys.argv[1]=="1": allowed|={"application-identifier","com.apple.developer.team-identifier"}
print(" ".join(sorted(k for k in d if k not in allowed)))' "$signed")
  [[ -z "$extra" ]] || bad "unexpected entitlements: $extra"
fi

# 4. No test-only code in the shipped binary.
scan=$(mktemp "${TMPDIR:-/tmp}/tf-strings.XXXXXX")
if ! strings -a "$app/$exe" > "$scan"; then
  bad 'strings failed while inspecting binary'
fi
if grep -E 'USBHostFixture|TEST-ONLY' "$scan" > /dev/null; then
  bad 'binary contains USBHostFixture or TEST-ONLY strings'
fi

rm -f "$scan"

# 5. The token is never persisted: no storage APIs in the app sources.
hits=$(grep -rEn 'UserDefaults|@AppStorage|SecItemAdd|SecItemUpdate|FileManager|FileHandle|createFile|\.write\(to|\.write\(toFile|NSKeyedArchiver|fopen|NSUbiquitousKeyValueStore' \
  "$src_root/App" "$src_root/Sources" 2>/dev/null || true)
[[ -z "$hits" ]] || bad "storage/file-write API in App or Sources (token must stay in memory):
$hits"

if (( fail )); then exit 1; fi
echo "PASS: $app (platform IOS, no background modes/ATS exception, entitlements limited to get-task-allow, no test-only strings, no token persistence API)"
