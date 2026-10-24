//go:build windows

package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
