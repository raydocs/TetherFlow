//go:build windows

package steamsetup

import (
	"errors"
	"syscall"
)

// O_NOFOLLOW does not exist on Windows; readJournalFile Lstat-checks and compares file identity instead.
const noFollowFlag = 0

// Transient errnos: 5 access denied, 32 sharing violation, 33 lock violation, 1224 user-mapped section open.
func transientOS(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 5, 32, 33, 1224:
			return true
		}
	}
	return false
}

// syncDir is a no-op: directories cannot be fsynced on Windows, and the file Sync covers contents.
func syncDir(string) {}

// checkDirTrusted relies on the hosts directory's admin-only ACL, which the journal inherits.
func checkDirTrusted(string) error { return nil }
