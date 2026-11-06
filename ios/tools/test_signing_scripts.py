"""Tests for build-signed.sh and verify-app.sh with xcodebuild/codesign/vtool/security stubbed on PATH.

No real Xcode, account, profile or device is touched; the stubs only record argv and fabricate a bundle.
"""
import os
import plistlib
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
REAL_IOS = TOOLS.parent
TEAM = "ABCDE12345"
BUNDLE = "com.example.tf"

STUBS = {
    "xcodegen": "#!/bin/bash\nexit 0\n",
    "xcodebuild": """#!/bin/bash
echo "$@" >> "$TF_STUB_DIR/xcodebuild.argv"
app=DerivedData/Build/Products/Release-iphoneos/TetherFlowIOS.app
mkdir -p "$app"
cp "$TF_STUB_DIR/Info.plist" "$app/Info.plist"
cp "$TF_STUB_DIR/exe" "$app/TetherFlowIOS"
[ -f "$TF_STUB_DIR/profile.in" ] && cp "$TF_STUB_DIR/profile.in" "$app/embedded.mobileprovision"
exit 0
""",
    "codesign": """#!/bin/bash
echo "$@" >> "$TF_STUB_DIR/codesign.argv"
if [ -f "$TF_STUB_DIR/entitlements.plist" ]; then cat "$TF_STUB_DIR/entitlements.plist"; exit 0; fi
echo "code object is not signed at all" >&2; exit 1
""",
    "security": "#!/bin/bash\ncat \"$TF_STUB_DIR/profile.plist\"\n",
    "vtool": "#!/bin/bash\nprintf 'Load command 1\\n      cmd LC_BUILD_VERSION\\n platform %s\\n' \"${TF_STUB_PLATFORM:-IOS}\"\n",
}


class Env:
    def __init__(self, case):
        self.root = Path(tempfile.mkdtemp(prefix="tf-signing-test-"))
        case.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        self.ios = self.root / "ios"
        (self.ios / "tools").mkdir(parents=True)
        for name in ("build-signed.sh", "verify-app.sh"):
            shutil.copy(TOOLS / name, self.ios / "tools" / name)
        self.stub = self.root / "stub"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.stub.mkdir()
        for name, body in STUBS.items():
            p = self.bin / name
            p.write_text(body)
            p.chmod(p.stat().st_mode | stat.S_IEXEC)
        self.write_plist("Info.plist", {"CFBundleExecutable": "TetherFlowIOS"})
        (self.stub / "exe").write_bytes(b"\x00binary\x00 ordinary strings only\x00")
        self.write_plist("profile.plist", {"TeamIdentifier": [TEAM],
                                           "Entitlements": {"application-identifier": f"{TEAM}.{BUNDLE}"}})
        (self.stub / "profile.in").write_bytes(b"signed-profile-blob")

    def write_plist(self, name, value):
        (self.stub / name).write_bytes(plistlib.dumps(value))

    def entitlements(self, **extra):
        value = {"application-identifier": f"{TEAM}.{BUNDLE}",
                 "com.apple.developer.team-identifier": TEAM}
        value.update(extra)
        self.write_plist("entitlements.plist", value)

    def run(self, script, *args, **env):
        e = {"PATH": f"{self.bin}:/usr/bin:/bin", "TF_STUB_DIR": str(self.stub),
             "TMPDIR": str(self.root), "HOME": str(self.root)}
        e.update(env)
        return subprocess.run([str(self.ios / "tools" / script), *args], env=e, cwd=self.root,
                              capture_output=True, text=True, timeout=60)

    def app(self):
        """A bundle as the stubbed xcodebuild would leave it, for verify-app.sh on its own."""
        app = self.root / "Fixture.app"
        app.mkdir(exist_ok=True)
        shutil.copy(self.stub / "Info.plist", app / "Info.plist")
        shutil.copy(self.stub / "exe", app / "TetherFlowIOS")
        return app

    def argv(self, name):
        p = self.stub / f"{name}.argv"
        return p.read_text() if p.exists() else ""


class SignedBuildTests(unittest.TestCase):
    def test_signed_build_refuses_without_team(self):
        env = Env(self)
        for team, bundle in [(None, BUNDLE), (TEAM, None), ("", BUNDLE), ("lowercase1", BUNDLE), (TEAM, "nodots")]:
            e = {}
            if team is not None:
                e["TF_TEAM_ID"] = team
            if bundle is not None:
                e["TF_BUNDLE_ID"] = bundle
            r = env.run("build-signed.sh", **e)
            self.assertNotEqual(r.returncode, 0, (team, bundle))
        self.assertEqual(env.argv("xcodebuild"), "", "xcodebuild must not run without valid team and bundle ID")

    def test_signed_build_uses_release_and_no_provisioning_by_default(self):
        env = Env(self)
        env.entitlements(**{"get-task-allow": False})
        r = env.run("build-signed.sh", TF_TEAM_ID=TEAM, TF_BUNDLE_ID=BUNDLE)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        argv = env.argv("xcodebuild")
        self.assertIn("-configuration Release", argv)
        self.assertIn("-scheme TetherFlowIOS", argv)
        self.assertIn("-destination generic/platform=iOS", argv)
        self.assertIn(f"DEVELOPMENT_TEAM={TEAM}", argv)
        self.assertIn(f"PRODUCT_BUNDLE_IDENTIFIER={BUNDLE}", argv)
        self.assertNotIn("-allowProvisioningUpdates", argv)
        self.assertNotIn("CODE_SIGNING_ALLOWED=NO", argv)
        self.assertIn("--entitlements", env.argv("codesign"))

        env2 = Env(self)
        env2.entitlements()
        r = env2.run("build-signed.sh", "--allow-provisioning", TF_TEAM_ID=TEAM, TF_BUNDLE_ID=BUNDLE)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("-allowProvisioningUpdates", env2.argv("xcodebuild"))

    def test_signed_build_fails_on_profile_for_other_team(self):
        env = Env(self)
        env.entitlements()
        env.write_plist("profile.plist", {"TeamIdentifier": ["ZZZZZ99999"],
                                          "Entitlements": {"application-identifier": f"{TEAM}.{BUNDLE}"}})
        r = env.run("build-signed.sh", TF_TEAM_ID=TEAM, TF_BUNDLE_ID=BUNDLE)
        self.assertNotEqual(r.returncode, 0)

    def test_signed_build_fails_without_embedded_profile(self):
        env = Env(self)
        env.entitlements()
        (env.stub / "profile.in").unlink()
        r = env.run("build-signed.sh", TF_TEAM_ID=TEAM, TF_BUNDLE_ID=BUNDLE)
        self.assertNotEqual(r.returncode, 0)


class VerifyAppTests(unittest.TestCase):
    def test_verify_app_accepts_clean_unsigned_app(self):
        env = Env(self)
        r = env.run("verify-app.sh", str(env.app()))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_verify_app_accepts_get_task_allow_only(self):
        env = Env(self)
        env.write_plist("entitlements.plist", {"get-task-allow": True})
        r = env.run("verify-app.sh", str(env.app()))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_verify_app_rejects_non_ios_platform(self):
        env = Env(self)
        r = env.run("verify-app.sh", str(env.app()), TF_STUB_PLATFORM="MACOS")
        self.assertNotEqual(r.returncode, 0)

    def test_verify_app_rejects_background_mode(self):
        env = Env(self)
        env.write_plist("Info.plist", {"CFBundleExecutable": "TetherFlowIOS", "UIBackgroundModes": ["audio"]})
        r = env.run("verify-app.sh", str(env.app()))
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("UIBackgroundModes", r.stderr)

    def test_verify_app_rejects_ats_exception(self):
        env = Env(self)
        env.write_plist("Info.plist", {"CFBundleExecutable": "TetherFlowIOS",
                                       "NSAppTransportSecurity": {"NSAllowsArbitraryLoads": True}})
        r = env.run("verify-app.sh", str(env.app()))
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("NSAppTransportSecurity", r.stderr)

    def test_verify_app_rejects_fixture_strings(self):
        for marker in (b"USBHostFixture", b"TEST-ONLY"):
            env = Env(self)
            (env.stub / "exe").write_bytes(b"\x00prefix " + marker + b" suffix\x00")
            r = env.run("verify-app.sh", str(env.app()))
            self.assertNotEqual(r.returncode, 0, marker)
            self.assertIn("TEST-ONLY", r.stderr)

    def test_verify_app_rejects_extra_entitlement(self):
        env = Env(self)
        env.write_plist("entitlements.plist", {"get-task-allow": True,
                                               "com.apple.developer.networking.networkextension": ["packet-tunnel-provider"]})
        r = env.run("verify-app.sh", str(env.app()))
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("unexpected entitlements", r.stderr)

    def test_verify_app_signed_flag_allows_only_identity_entitlements(self):
        env = Env(self)
        env.entitlements(**{"get-task-allow": True})
        self.assertNotEqual(env.run("verify-app.sh", str(env.app())).returncode, 0)
        self.assertEqual(env.run("verify-app.sh", "--signed", str(env.app())).returncode, 0)
        env.entitlements(**{"aps-environment": "development"})
        self.assertNotEqual(env.run("verify-app.sh", "--signed", str(env.app())).returncode, 0)

    def test_verify_app_rejects_token_persistence_apis(self):
        for api in ("UserDefaults.standard.set(t, forKey: k)", "SecItemAdd(q, nil)",
                    "try s.write(to: url)", "FileManager.default.createFile(atPath: p, contents: d)"):
            env = Env(self)
            src = env.root / "src"
            (src / "App").mkdir(parents=True)
            (src / "Sources").mkdir()
            (src / "App" / "X.swift").write_text(f"func f() {{ {api} }}\n")
            r = env.run("verify-app.sh", "--src-root", str(src), str(env.app()))
            self.assertNotEqual(r.returncode, 0, api)
            self.assertIn("X.swift", r.stderr)

    def test_real_sources_have_no_token_persistence_api(self):
        env = Env(self)
        r = env.run("verify-app.sh", "--src-root", str(REAL_IOS), str(env.app()))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
