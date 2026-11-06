//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const createNewConsole = 0x00000010

func consoleModeOf(f *os.File) (uint32, error) {
	var mode uint32
	err := syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode)
	return mode, err
}

// TestHiddenInputClearsEchoAndRestores runs a helper in its own console (so the
// test never changes the console of the test runner), reads the input mode
// through CONIN$ and checks it before, during and after withHiddenInput, also
// when the callback panics.
func TestHiddenInputClearsEchoAndRestores(t *testing.T) {
	out := filepath.Join(t.TempDir(), "modes.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHiddenInputConsoleHelper$")
	cmd.Env = append(os.Environ(), "TF_HIDDEN_HELPER=1", "TF_HIDDEN_OUT="+out)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper timed out")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint32{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var key string
		var val uint32
		if _, err := fmt.Sscanf(line, "%s %d", &key, &val); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		got[key] = val
	}
	if got["before"]&enableEchoInput == 0 {
		t.Fatalf("helper console did not start with echo on: %#x", got["before"])
	}
	if got["during"]&enableEchoInput != 0 {
		t.Fatalf("echo still on inside withHiddenInput: %#x", got["during"])
	}
	if got["during"] != got["before"]&^enableEchoInput {
		t.Fatalf("other console flags changed: before %#x during %#x", got["before"], got["during"])
	}
	if got["after"] != got["before"] {
		t.Fatalf("mode not restored: before %#x after %#x", got["before"], got["after"])
	}
	if got["afterpanic"] != got["before"] {
		t.Fatalf("mode not restored after panic: before %#x after %#x", got["before"], got["afterpanic"])
	}
}

func TestHiddenInputConsoleHelper(t *testing.T) {
	if os.Getenv("TF_HIDDEN_HELPER") != "1" {
		t.Skip("helper process")
	}
	conin, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conin.Close()
	var lines []string
	record := func(key string) {
		mode, err := consoleModeOf(conin)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%s %d", key, mode))
	}
	record("before")
	if err := withHiddenInput(conin, func() { record("during") }); err != nil {
		t.Fatal(err)
	}
	record("after")
	func() {
		defer func() { _ = recover() }()
		_ = withHiddenInput(conin, func() { panic("callback failed") })
	}()
	record("afterpanic")
	if err := os.WriteFile(os.Getenv("TF_HIDDEN_OUT"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
