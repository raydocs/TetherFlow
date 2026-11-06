package steamsetup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const crashExitCode = 77
const crashBase = "# base\n127.0.0.1 localhost\n"

// The helper runs in a real child process and dies inside the fault hook, so the
// parent observes exactly what a power-cut-like process death leaves on disk.
func TestCrashHelperProcess(t *testing.T) {
	stage := os.Getenv("TF_CRASH_STAGE")
	if stage == "" {
		t.Skip("helper process only")
	}
	faultHook = func(s string) {
		if s == stage {
			os.Exit(crashExitCode)
		}
	}
	_, err := Update(os.Getenv("TF_CRASH_PATH"), os.Getenv("TF_CRASH_IP"), lease)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func crashChild(t *testing.T, path, ip, stage string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$")
	cmd.Env = append(os.Environ(), "TF_CRASH_STAGE="+stage, "TF_CRASH_PATH="+path, "TF_CRASH_IP="+ip)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != crashExitCode {
		t.Fatalf("child did not crash at %s: %v\n%s", stage, err, out)
	}
}

func TestCrashAtEachStageRecoversOnNextUpdate(t *testing.T) {
	for _, op := range []string{"install", "remove"} {
		for _, stage := range []string{"after-journal", "mid-write", "after-write", "after-sync"} {
			t.Run(op+"/"+stage, func(t *testing.T) {
				dir := t.TempDir()
				p := filepath.Join(dir, "hosts")
				if err := os.WriteFile(p, []byte(crashBase), 0600); err != nil {
					t.Fatal(err)
				}
				ip, want := "192.168.1.5", ""
				installed, err := Rewrite([]byte(crashBase), ip, lease)
				if err != nil {
					t.Fatal(err)
				}
				if op == "install" {
					want = string(installed)
				} else {
					if err := os.WriteFile(p, installed, 0600); err != nil {
						t.Fatal(err)
					}
					ip, want = "", crashBase
				}
				crashChild(t, p, ip, stage)
				if _, err := Update(p, ip, lease); err != nil {
					t.Fatalf("update after crash at %s: %v", stage, err)
				}
				got, _ := os.ReadFile(p)
				if string(got) != want {
					t.Fatalf("after recovery:\n%q\nwant:\n%q", got, want)
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 1 {
					t.Fatalf("leftover files after recovery: %v", entries)
				}
			})
		}
	}
}
