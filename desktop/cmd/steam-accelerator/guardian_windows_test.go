//go:build windows

package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"tetherflow-desktop/internal/steamsetup"
)

var probeParentHandle = flag.Uint64("parent-handle", 0, "test inherited handle")

// Test the exact production inherited-handle setup with a parent that exits
// immediately, without touching the real Windows hosts file.
func TestInheritedGuardianSurvivesImmediateParentExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guardian-finished")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedGuardianHelper$")
	cmd.Env = append(os.Environ(), "TF_GUARDIAN_PROBE=parent", "TF_GUARDIAN_PROBE_PATH="+path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("parent helper: %v %s", e, out)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, e := os.ReadFile(path); e == nil {
			if string(data) != "parent-exited" {
				t.Fatal(string(data))
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("inherited-handle guardian did not observe parent exit")
}
func TestInheritedGuardianHelper(t *testing.T) {
	switch os.Getenv("TF_GUARDIAN_PROBE") {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedGuardianHelper$")
		// Filter the inherited helper mode, not the production process configuration.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "TF_GUARDIAN_PROBE=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "TF_GUARDIAN_PROBE=guardian", "TF_GUARDIAN_PARENT="+strconv.Itoa(os.Getpid()))
		release, e := prepareGuardian(cmd)
		if e != nil {
			os.Exit(2)
		}
		e = cmd.Start()
		release()
		if e != nil {
			os.Exit(3)
		}
		_ = cmd.Process.Release()
		os.Exit(0)
	case "guardian":
		pid, e := strconv.Atoi(os.Getenv("TF_GUARDIAN_PARENT"))
		if e != nil {
			os.Exit(4)
		}
		if e = waitParent(pid, *probeParentHandle); e != nil {
			os.Exit(5)
		}
		if e = os.WriteFile(os.Getenv("TF_GUARDIAN_PROBE_PATH"), []byte("parent-exited"), 0600); e != nil {
			os.Exit(6)
		}
		os.Exit(0)
	}
}

// The tests below run the production guardian (prepareGuardian, inherited handle, guardPath)
// against a temporary hosts file. The parent is a helper process that the test kills.
const (
	recoveryTestLease = "0123456789abcdef0123456789abcdef"
	recoveryTestBase  = "127.0.0.1 localhost\r\n# keep me\r\n"
)

type recoveryRun struct {
	hosts, logPath, donePath, flushPath string
	parent                              *exec.Cmd
}

func startRecoveryParent(t *testing.T) *recoveryRun {
	t.Helper()
	dir := t.TempDir()
	r := &recoveryRun{hosts: filepath.Join(dir, "hosts"), logPath: filepath.Join(dir, "recovery.log"), donePath: filepath.Join(dir, "done"), flushPath: filepath.Join(dir, "flush")}
	if e := os.WriteFile(r.hosts, []byte(recoveryTestBase), 0o644); e != nil {
		t.Fatal(e)
	}
	ready := filepath.Join(dir, "ready")
	r.parent = exec.Command(os.Args[0], "-test.run=^TestRecoveryGuardianHelper$")
	r.parent.Env = append(os.Environ(), "TF_RECOVERY_PROBE=parent", "TF_RECOVERY_HOSTS="+r.hosts, "TF_RECOVERY_LEASE="+recoveryTestLease,
		"TF_RECOVERY_LOG="+r.logPath, "TF_RECOVERY_DONE="+r.donePath, "TF_RECOVERY_FLUSH="+r.flushPath, "TF_RECOVERY_READY="+ready)
	if e := r.parent.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = r.parent.Process.Kill(); _ = r.parent.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, e := os.Stat(ready); e == nil {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("parent helper never became ready")
	return nil
}

// kill terminates the parent without any cleanup, unlike the clean exit of the older test.
func (r *recoveryRun) kill(t *testing.T) {
	t.Helper()
	if e := r.parent.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = r.parent.Wait()
}
func (r *recoveryRun) waitDone(t *testing.T, limit time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if data, e := os.ReadFile(r.donePath); e == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("guardian did not finish")
	return ""
}
func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func appendTestFile(t *testing.T, p, text string) {
	t.Helper()
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.WriteString(text); e != nil {
		t.Fatal(e)
	}
}

func TestGuardianRemovesBlockAfterForcedParentKill(t *testing.T) {
	r := startRecoveryParent(t)
	if !strings.Contains(readTestFile(t, r.hosts), "BEGIN TetherFlow Steam "+recoveryTestLease) {
		t.Fatal("parent did not install the block")
	}
	const foreign = "10.9.9.9 foreign.example\r\n"
	appendTestFile(t, r.hosts, foreign) // foreign edit made while the parent was alive
	r.kill(t)
	if got := r.waitDone(t, 30*time.Second); got != "ok" {
		t.Fatalf("guardian: %s", got)
	}
	if got := readTestFile(t, r.hosts); got != recoveryTestBase+foreign {
		t.Fatalf("hosts = %q", got)
	}
	if strings.Count(readTestFile(t, r.flushPath), "flush") != 1 {
		t.Fatal("DNS flush should run once after the change")
	}
	if _, e := os.Stat(r.logPath); e == nil {
		t.Fatal("successful cleanup wrote a recovery log")
	}
}

func TestGuardianRetriesTransientLock(t *testing.T) {
	r := startRecoveryParent(t)
	name, e := syscall.UTF16PtrFromString(r.hosts)
	if e != nil {
		t.Fatal(e)
	}
	// Write access without FILE_SHARE_WRITE makes the guardian's open fail with a sharing violation (32).
	h, e := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		t.Fatal(e)
	}
	released := false
	defer func() {
		if !released {
			syscall.CloseHandle(h)
		}
	}()
	r.kill(t)
	time.Sleep(1500 * time.Millisecond)
	if _, e := os.Stat(r.donePath); e == nil {
		t.Fatalf("guardian gave up while the lock was held: %s", readTestFile(t, r.donePath))
	}
	if !strings.Contains(readTestFile(t, r.hosts), "BEGIN TetherFlow Steam") {
		t.Fatal("block removed while the file was locked")
	}
	syscall.CloseHandle(h)
	released = true
	if got := r.waitDone(t, 30*time.Second); got != "ok" {
		t.Fatalf("guardian: %s", got)
	}
	if got := readTestFile(t, r.hosts); got != recoveryTestBase {
		t.Fatalf("hosts = %q", got)
	}
	if _, e := os.Stat(r.logPath); e == nil {
		t.Fatal("retried cleanup left a recovery log")
	}
}

func TestGuardianPermanentRefusalWritesRecoveryLog(t *testing.T) {
	r := startRecoveryParent(t)
	// A foreign record inside the block makes removal a permanent refusal.
	tampered := strings.Replace(readTestFile(t, r.hosts), "# END TetherFlow Steam", "6.6.6.6 evil.example\r\n# END TetherFlow Steam", 1)
	if e := os.WriteFile(r.hosts, []byte(tampered), 0o644); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	r.kill(t)
	got := r.waitDone(t, 20*time.Second)
	if !strings.HasPrefix(got, "err:") {
		t.Fatalf("guardian: %s", got)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("permanent refusal was retried")
	}
	if readTestFile(t, r.hosts) != tampered {
		t.Fatal("refused cleanup changed hosts")
	}
	logged := readTestFile(t, r.logPath)
	if !strings.Contains(logged, "lease="+recoveryTestLease) || !strings.Contains(logged, "--recover-lease "+recoveryTestLease) {
		t.Fatalf("recovery log: %q", logged)
	}
	if _, e := os.Stat(r.flushPath); e == nil {
		t.Fatal("flushed DNS although nothing changed")
	}
}

func TestRecoveryGuardianHelper(t *testing.T) {
	lease, hosts := os.Getenv("TF_RECOVERY_LEASE"), os.Getenv("TF_RECOVERY_HOSTS")
	switch os.Getenv("TF_RECOVERY_PROBE") {
	case "parent":
		if _, e := steamsetup.Update(hosts, "192.168.1.5", lease); e != nil {
			os.Exit(2)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryGuardianHelper$")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "TF_RECOVERY_PROBE=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "TF_RECOVERY_PROBE=guardian", "TF_GUARDIAN_PARENT="+strconv.Itoa(os.Getpid()))
		release, e := prepareGuardian(cmd)
		if e != nil {
			os.Exit(3)
		}
		e = cmd.Start()
		release()
		if e != nil {
			os.Exit(4)
		}
		_ = cmd.Process.Release()
		if e = os.WriteFile(os.Getenv("TF_RECOVERY_READY"), []byte("ready"), 0o600); e != nil {
			os.Exit(5)
		}
		time.Sleep(10 * time.Minute) // the test kills this process
		os.Exit(6)
	case "guardian":
		pid, e := strconv.Atoi(os.Getenv("TF_GUARDIAN_PARENT"))
		if e != nil {
			os.Exit(7)
		}
		flush := func() error {
			f, e := os.OpenFile(os.Getenv("TF_RECOVERY_FLUSH"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if e != nil {
				return e
			}
			defer f.Close()
			_, e = f.WriteString("flush\n")
			return e
		}
		result := "ok"
		if e = guardPath(pid, lease, *probeParentHandle, hosts, flush, os.Getenv("TF_RECOVERY_LOG")); e != nil {
			result = "err: " + e.Error()
		}
		tmp := os.Getenv("TF_RECOVERY_DONE") + ".tmp"
		if e = os.WriteFile(tmp, []byte(result), 0o600); e != nil || os.Rename(tmp, os.Getenv("TF_RECOVERY_DONE")) != nil {
			os.Exit(8)
		}
		os.Exit(0)
	}
}
