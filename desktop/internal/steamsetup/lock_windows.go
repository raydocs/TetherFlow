//go:build windows

package steamsetup

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var lock = kernel.NewProc("LockFileEx")
var unlock = kernel.NewProc("UnlockFileEx")

func lockFile(f *os.File) error {
	var ov syscall.Overlapped
	r, _, err := lock.Call(f.Fd(), 3, 0, 0xffffffff, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		return err
	}
	return nil
}
func unlockFile(f *os.File) {
	var ov syscall.Overlapped
	unlock.Call(f.Fd(), 0, 0xffffffff, 0, uintptr(unsafe.Pointer(&ov)))
}
