#!/bin/bash
# Signed Release build, run by the user on their own Mac. Agents and CI never run this:
# it needs a real Apple team. Usage: TF_TEAM_ID=... TF_BUNDLE_ID=... build-signed.sh [--allow-provisioning]
set -euo pipefail
cd "$(dirname "$0")/.."
allow=()
case "${1:-}" in
  '') ;;
  --allow-provisioning) allow=(-allowProvisioningUpdates) ;;
  *) echo 'usage: TF_TEAM_ID=... TF_BUNDLE_ID=... build-signed.sh [--allow-provisioning]' >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || { echo 'too many arguments' >&2; exit 2; }
[[ -n "${TF_TEAM_ID:-}" ]] || { echo 'TF_TEAM_ID is required (your Apple Developer Team ID)' >&2; exit 2; }
[[ -n "${TF_BUNDLE_ID:-}" ]] || { echo 'TF_BUNDLE_ID is required (a unique bundle identifier)' >&2; exit 2; }
[[ "$TF_TEAM_ID" =~ ^[A-Z0-9]{10}$ ]] || { echo 'TF_TEAM_ID must be 10 uppercase letters/digits' >&2; exit 2; }
[[ "$TF_BUNDLE_ID" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] ||
  { echo 'TF_BUNDLE_ID must be a reverse-DNS identifier' >&2; exit 2; }

mkdir -p logs
xcodegen generate --spec project.yml 2>&1 | tee logs/xcodegen.log
xcodebuild -project TetherFlowIOS.xcodeproj -scheme TetherFlowIOS \
  -configuration Release -destination 'generic/platform=iOS' \
  -derivedDataPath DerivedData ${allow[@]+"${allow[@]}"} \
  DEVELOPMENT_TEAM="$TF_TEAM_ID" PRODUCT_BUNDLE_IDENTIFIER="$TF_BUNDLE_ID" CODE_SIGN_STYLE=Automatic \
  build 2>&1 | tee logs/xcodebuild-signed.log

app=DerivedData/Build/Products/Release-iphoneos/TetherFlowIOS.app
[[ -d "$app" ]] || { echo "FAIL: $app was not produced" >&2; exit 1; }
profile="$app/embedded.mobileprovision"
[[ -f "$profile" ]] || { echo 'FAIL: no embedded provisioning profile' >&2; exit 1; }

work=$(mktemp -d "${TMPDIR:-/tmp}/tetherflow-signed.XXXXXX")
trap 'rm -rf "$work"' EXIT
codesign -d --entitlements :- "$app" > "$work/entitlements.plist" 2> "$work/codesign.err" ||
  { echo 'FAIL: codesign could not read entitlements (app unsigned?)' >&2; exit 1; }
security cms -D -i "$profile" > "$work/profile.plist" ||
  { echo 'FAIL: embedded profile could not be decoded' >&2; exit 1; }

python3 -I - "$work/entitlements.plist" "$work/profile.plist" "$TF_TEAM_ID" "$TF_BUNDLE_ID" <<'PY'
import plistlib, sys
ent_path, prof_path, team, bundle = sys.argv[1:5]
raw = open(ent_path, "rb").read()
try:
    ent = plistlib.loads(raw[raw.index(b"<?xml"):])
except Exception:
    sys.exit("FAIL: entitlements are not a readable plist")
prof = plistlib.loads(open(prof_path, "rb").read())
errors = []
if team not in prof.get("TeamIdentifier", []):
    errors.append("profile TeamIdentifier does not include TF_TEAM_ID")
if ent.get("application-identifier") != f"{team}.{bundle}":
    errors.append("application-identifier is not TF_TEAM_ID.TF_BUNDLE_ID")
allowed = {"application-identifier", "com.apple.developer.team-identifier", "get-task-allow"}
extra = sorted(set(ent) - allowed)
if extra:
    errors.append("unexpected entitlements: " + " ".join(extra))
pent = prof.get("Entitlements", {})
if pent.get("application-identifier") not in (f"{team}.{bundle}", f"{team}.*"):
    errors.append("profile does not cover this application-identifier")
if errors:
    sys.exit("FAIL: " + "; ".join(errors))
print("entitlements and embedded profile match the requested team and bundle ID")
PY
./tools/verify-app.sh --signed "$app"
echo "PASS: signed Release build at ios/$app. Install and run it yourself (hardware gate H1); this is not an installed-device test."
