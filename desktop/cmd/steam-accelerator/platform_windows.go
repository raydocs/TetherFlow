//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Open the parent handle before CreateProcess and inherit it into the guardian.
// Unlike reopening a PID after launch, this also survives immediate parent exit.
func prepareGuardian(cmd *exec.Cmd) (func(), error) {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_QUERY_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return nil, err
	}
	if err = syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		syscall.CloseHandle(h)
		return nil, err
	}
	cmd.Args = append(cmd.Args, "--parent-handle", strconv.FormatUint(uint64(h), 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, AdditionalInheritedHandles: []syscall.Handle{h}}
	return func() { _ = syscall.CloseHandle(h) }, nil
}
func waitParent(pid int, handle uint64) error {
	if pid <= 0 || pid == os.Getpid() || handle == 0 {
		return errors.New("invalid guardian parent handle")
	}
	h := syscall.Handle(handle)
	defer syscall.CloseHandle(h)
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetProcessId")
	actual, _, err := proc.Call(uintptr(h))
	if actual == 0 {
		return fmt.Errorf("invalid inherited process handle: %w", err)
	}
	if actual != uintptr(pid) {
		return errors.New("inherited process handle identity mismatch")
	}
	result, err := syscall.WaitForSingleObject(h, syscall.INFINITE)
	if err != nil {
		return fmt.Errorf("cannot wait for parent: %w", err)
	}
	if result != syscall.WAIT_OBJECT_0 {
		return fmt.Errorf("unexpected parent wait result: %d", result)
	}
	return nil
}
func guard(pid int, lease string, handle uint64) error {
	if e := waitParent(pid, handle); e != nil {
		return e
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
