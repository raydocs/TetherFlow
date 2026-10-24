//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"tetherflow-desktop/internal/steamsetup"
	"unsafe"
)

func windowsDirectory() (string, error) {
	buf := make([]uint16, 32768)
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetWindowsDirectoryW")
	n, _, e := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return "", e
	}
	if n >= uintptr(len(buf)) {
		return "", errors.New("Windows directory too long")
	}
	return syscall.UTF16ToString(buf[:n]), nil
}
func hostsPath() (string, error) {
	dir, e := windowsDirectory()
	if e != nil {
		return "", e
	}
	return filepath.Join(dir, "System32", "drivers", "etc", "hosts"), nil
}
func flushDNS() error {
	dir, e := windowsDirectory()
	if e != nil {
		return e
	}
	return exec.Command(filepath.Join(dir, "System32", "ipconfig.exe"), "/flushdns").Run()
}
func prepareGuardian(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
}
func guard(pid int, lease string) error {
	if pid <= 0 || pid == os.Getpid() {
		return errors.New("invalid guardian parent")
	}
	p, e := os.FindProcess(pid)
	if e != nil {
		return fmt.Errorf("cannot hold parent handle: %w", e)
	}
	// Windows FindProcess holds a process handle: PID reuse cannot change the waited process.
	if _, e = p.Wait(); e != nil {
		return fmt.Errorf("cannot wait for parent: %w", e)
	}
	path, e := hostsPath()
	if e != nil {
		return e
	}
	changed, e := steamsetup.Update(path, "", lease)
	if e != nil {
		return e
	}
	if changed {
		return flushDNS()
	}
	return nil
}
